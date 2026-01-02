package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	jobs   = make(map[string]*Job)
	jobsMu sync.Mutex

	idem   = make(map[string]string) //idem_key -> job_id
	idemMu sync.Mutex

	jobCond = sync.NewCond(&jobsMu)
)

type PollRequest struct {
	WorkerID string `json:"worker_id"`
}

type EnqueueRequest struct {
	Payload string `json:"payload"`
}

type AckRequest struct {
	WorkerID string `json:"worker_id"`
	JobID    string `json:"job_id"`
	LeaseID  int64  `json:"lease_id"`
}

type FailRequest struct {
	WorkerID string `json:"worker_id"`
	JobID    string `json:"job_id"`
	LeaseID  int64  `json:"lease_id"`
}

type JobState string

const (
	StateQueued JobState = "QUEUED"
	StateLeased JobState = "LEASED"
	StateDone   JobState = "DONE"
	StateDead   JobState = "DEAD"
)

type Job struct {
	ID      string   `json:"id"`
	Payload string   `json:"payload"`
	State   JobState `json:"state"`

	LeaseOwner     string `json:"lease_owner,omitempty"`
	LeaseExpiresAt int64  `json:"lease_expires_at,omitempty"`
	LeaseID        int64  `json:"lease_id"`

	Attempts        int   `json:"attempts"`
	MaxTries        int   `json:"max_tries"`
	NextAvailableAt int64 `json:"next_available_at,omitempty"`
}

func expireLeases(now int64) []string {
	var expiredIDs []string = make([]string, 0)

	jobsMu.Lock()
	defer jobsMu.Unlock()

	for id, job := range jobs {
		if job.State == StateLeased && job.LeaseExpiresAt > 0 && job.LeaseExpiresAt <= now {
			job.State = StateQueued
			job.LeaseOwner = ""
			job.LeaseExpiresAt = 0
			job.NextAvailableAt = 0
			expiredIDs = append(expiredIDs, id)
		}
	}

	for _, id := range expiredIDs {
		logEvent("lease_expired", map[string]interface{}{
			"job_id": id,
		})
	}

	return expiredIDs
}

// exponential back off and jitter
func retryDelaySeconds(attempts int) int64 {
	if attempts < 1 {
		attempts = 1
	}

	const base int64 = 5
	const capDelay int64 = 30

	delay := base << int64(attempts-1)
	if delay > capDelay {
		delay = capDelay
	}

	return rand.Int63n(delay + 1)
}

func logEvent(event string, fields map[string]interface{}) {
	msg := fmt.Sprintf("event=%s", event)
	for k, v := range fields {
		msg += fmt.Sprintf(" %s=%v", k, v)
	}
	log.Println(msg)
}

func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	http.HandleFunc("/enqueue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		idemKey := r.Header.Get("Idempotency-Key")
		if idemKey != "" {
			idemMu.Lock()
			existingID, ok := idem[idemKey]

			if ok && existingID != "PENDING" {
				idemMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"job_id":"%s"}`, existingID)
				return
			}

			if ok && existingID == "PENDING" {
				idemMu.Unlock()
				w.WriteHeader(http.StatusConflict)
				return
			}

			idem[idemKey] = "PENDING"
			idemMu.Unlock()
		}

		var req EnqueueRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			if idemKey != "" {
				idemMu.Lock()
				existing, ok := idem[idemKey]
				if ok && existing == "PENDING" {
					delete(idem, idemKey)
				}
				idemMu.Unlock()
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var id string = uuid.NewString()

		var job *Job = &Job{
			ID:       id,
			Payload:  req.Payload,
			State:    StateQueued,
			MaxTries: 3,
		}

		jobsMu.Lock()
		defer jobsMu.Unlock()
		jobs[id] = job
		jobCond.Signal()

		if idemKey != "" {
			idemMu.Lock()
			idem[idemKey] = id
			idemMu.Unlock()
		}

		logEvent("job_enqueued", map[string]interface{}{
			"job_id":          id,
			"payload_len":     len(job.Payload),
			"idempotency_key": idemKey,
		})

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"job_id":"%s"}`, id)
	})

	http.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		jobsMu.Lock()
		defer jobsMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jobs)
	})

	http.HandleFunc("/dead", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		jobsMu.Lock()
		defer jobsMu.Unlock()

		dead := make(map[string]*Job)
		for id, job := range jobs {
			if job.State == StateDead {
				dead[id] = job
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dead)
	})

	http.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req PollRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || req.WorkerID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		timeout := 30 * time.Second
		deadline := time.Now().Add(timeout)

		jobsMu.Lock()
		defer jobsMu.Unlock()

		for {
			now := time.Now().Unix()

			for _, job := range jobs {
				if job.State == StateQueued && (job.NextAvailableAt == 0 || job.NextAvailableAt <= now) {
					job.State = StateLeased
					job.LeaseOwner = req.WorkerID
					job.LeaseExpiresAt = now + 30
					job.LeaseID++

					logEvent("job_leased", map[string]interface{}{
						"job_id":           job.ID,
						"worker_id":        req.WorkerID,
						"lease_id":         job.LeaseID,
						"lease_expires_at": job.LeaseExpiresAt,
					})

					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(job)
					return
				}
			}

			//when no job available
			remaining := time.Until(deadline)
			if remaining <= 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			timer := time.AfterFunc(remaining, func() {
				jobsMu.Lock()
				jobCond.Signal()
				jobsMu.Unlock()
			})

			jobCond.Wait()
			timer.Stop()

		}
	})

	http.HandleFunc("/ack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req AckRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || req.WorkerID == "" || req.JobID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		jobsMu.Lock()
		defer jobsMu.Unlock()

		job, ok := jobs[req.JobID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if job.State == StateDone {
			w.WriteHeader(http.StatusOK)
			return
		}

		if job.State != StateLeased || job.LeaseOwner != req.WorkerID {
			logEvent("ack_rejected", map[string]interface{}{
				"job_id":      job.ID,
				"reason":      "not_current_lease_owner",
				"worker_id":   req.WorkerID,
				"lease_owner": job.LeaseOwner,
			})
			w.WriteHeader(http.StatusConflict)
			return
		}

		if req.LeaseID != job.LeaseID {
			logEvent("ack_rejected", map[string]interface{}{
				"job_id":           job.ID,
				"reason":           "stale_lease_id",
				"worker_lease_id":  req.LeaseID,
				"current_lease_id": job.LeaseID,
			})
			w.WriteHeader(http.StatusConflict)
			return
		}

		if job.LeaseExpiresAt <= time.Now().Unix() {
			logEvent("ack_rejected", map[string]interface{}{
				"job_id":           job.ID,
				"reason":           "lease_expired",
				"lease_expires_at": job.LeaseExpiresAt,
				"now":              time.Now().Unix(),
			})
			w.WriteHeader(http.StatusConflict)
			return
		}

		//mark done
		job.State = StateDone
		job.LeaseOwner = ""
		job.LeaseExpiresAt = 0

		logEvent("job_acked", map[string]interface{}{
			"job_id":    job.ID,
			"worker_id": req.WorkerID,
			"lease_id":  req.LeaseID,
		})

		w.WriteHeader(http.StatusOK)

	})

	http.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req FailRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil || req.WorkerID == "" || req.JobID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		jobsMu.Lock()
		defer jobsMu.Unlock()

		job, ok := jobs[req.JobID]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if job.State == StateDone {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Must be leased to this worker
		if job.State != StateLeased || job.LeaseOwner != req.WorkerID {
			w.WriteHeader(http.StatusConflict)
			return
		}

		if req.LeaseID != job.LeaseID {
			w.WriteHeader(http.StatusConflict)
			return
		}

		// Must not be expired
		now := time.Now().Unix()
		if job.LeaseExpiresAt <= now {
			w.WriteHeader(http.StatusConflict)
			return
		}

		// Record failure
		job.Attempts++

		logEvent("job_failed", map[string]interface{}{
			"job_id":    job.ID,
			"worker_id": req.WorkerID,
			"lease_id":  req.LeaseID,
			"attempts":  job.Attempts,
		})

		// Too many tries => DEAD (DLQ behavior)
		if job.Attempts >= job.MaxTries {
			job.State = StateDead
			job.LeaseOwner = ""
			job.LeaseExpiresAt = 0
			job.NextAvailableAt = 0

			logEvent("job_dead", map[string]interface{}{
				"job_id":   job.ID,
				"attempts": job.Attempts,
			})

			w.WriteHeader(http.StatusOK)
			return
		}

		// Retry later with backoff + full jitter
		delay := retryDelaySeconds(job.Attempts)

		job.State = StateQueued
		job.LeaseOwner = ""
		job.LeaseExpiresAt = 0
		job.NextAvailableAt = now + delay

		logEvent("job_retry_scheduled", map[string]interface{}{
			"job_id":            job.ID,
			"attempts":          job.Attempts,
			"next_available_at": job.NextAvailableAt,
		})

		w.WriteHeader(http.StatusOK)

	})

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			expireLeases(time.Now().Unix())
		}
	}()

	log.Println("Listening on port 8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
