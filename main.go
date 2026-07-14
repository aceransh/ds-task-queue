package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"database/sql"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

var (
	jobs   = make(map[string]*Job)
	jobsMu sync.Mutex

	idem   = make(map[string]string) //idem_key -> job_id
	idemMu sync.Mutex

	jobCond = sync.NewCond(&jobsMu)

	db *sql.DB
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

func initDB() {
	var err error
	db, err = sql.Open("postgres", "host=localhost port=5432 user=postgres password=db_password dbname=myDB sslmode=disable")
	if err != nil {
		log.Fatal("failed to open db: ", err)
	}

	err = db.Ping()
	if err != nil {
		log.Fatal("Failed to ping db: ", err)
	}

	log.Println("Successfully connected to the database!")
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
			jobCond.Signal()
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

	return rand.Int63n(delay + 1) //random delay between 0 and delay
}

func logEvent(event string, fields map[string]any) {
	msg := fmt.Sprintf("event=%s", event)
	for k, v := range fields {
		msg += fmt.Sprintf(" %s=%v", k, v)
	}
	log.Println(msg)
}

func main() {
	initDB()

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
			// 1. Try to lock the key by inserting PENDING
			result, err := db.Exec(`
				INSERT INTO idempotency_keys (idem_key, status) 
				VALUES ($1, 'PENDING') 
				ON CONFLICT (idem_key) DO NOTHING;
			`, idemKey)
			if err != nil {
				log.Println("Insert idempotency error:", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			rows, _ := result.RowsAffected()

			// 2. If rows == 0, the key already existed. We need to check its status.
			if rows == 0 {
				var status string
				var existingJobID sql.NullString // Safely handles NULLs!

				err := db.QueryRow("SELECT status, job_id FROM idempotency_keys WHERE idem_key = $1", idemKey).Scan(&status, &existingJobID)
				if err != nil {
					log.Println("Query idempotency error:", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				if status == "PENDING" {
					w.WriteHeader(http.StatusConflict)
					return
				}

				// It must be COMPLETED, so return the existing job ID
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"job_id":"%s"}`, existingJobID.String)
				return
			}
		}

		var req EnqueueRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			log.Println("Request error: ", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var id string = uuid.NewString()

		tx, err := db.Begin()
		if err != nil {
			log.Println("Error with db.Begin(): ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		_, err = tx.Exec("INSERT INTO Jobs (id, payload, state) VALUES ($1, $2, $3)", id, req.Payload, StateQueued)
		if err != nil {
			log.Println("Failed to insert job: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return // This triggers defer tx.Rollback()
		}
		if idemKey != "" {
			_, err = tx.Exec("UPDATE Idems SET status = 'COMPLETED', job_id = $1 WHERE idem_key = $2", id, idemKey)
			if err != nil {
				log.Println("Failed to update idem_key: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return // This triggers defer tx.Rollback()
			}
		}

		err = tx.Commit()
		if err != nil {
			log.Println("Failed to commit tx: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return // This triggers defer tx.Rollback()
		}

		jobsMu.Lock()
		jobCond.Signal()
		jobsMu.Unlock()

		logEvent("job_enqueued", map[string]interface{}{
			"job_id":          id,
			"payload_len":     len(req.Payload),
			"idempotency_key": idemKey,
		})

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"job_id":"%s"}`, id)
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

		for {
			now := time.Now().Unix()

			var job Job

			err := db.QueryRow(` 
			UPDATE Jobs
			SET state = 'LEASED',
				lease_owner = $1,
				lease_expires_at = $2,
				lease_id = lease_id + 1
			WHERE id = (
				SELECT id from Jobs
				WHERE state = 'QUEUED'
					AND (next_available_at = 0 OR next_available_at <= $3)
				FOR UPDATE SKIP LOCKED
				LIMIT 1
			)
			RETURNING id, payload, attempts, max_tries, lease_id, state, lease_owner, lease_expires_at;
			`, req.WorkerID, now+30, now).Scan(&job.ID, &job.Payload, &job.Attempts, &job.MaxTries, &job.LeaseID, &job.State, &job.LeaseOwner, &job.LeaseExpiresAt) //this basically just takes the first available job that isn't being looked at by another worker and set it to leased and gives it all its fields lol

			switch err {
			case nil:
				logEvent("job_leased", map[string]interface{}{
					"job_id":           job.ID,
					"worker_id":        req.WorkerID,
					"lease_id":         job.LeaseID,
					"lease_expires_at": job.LeaseExpiresAt,
				})
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(job)
				return
			case sql.ErrNoRows: //when no job available
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

				jobsMu.Lock()
				jobCond.Wait()
				jobsMu.Unlock()

				timer.Stop()
			default:
				log.Println("Failed to poll for job: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

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

		if job.State == StateDone { //already been acked
			w.WriteHeader(http.StatusOK)
			return
		}

		if job.State != StateLeased || job.LeaseOwner != req.WorkerID { //if either the state isn't leased or the leaseowner doesn't match the worker id from the request
			logEvent("ack_rejected", map[string]interface{}{
				"job_id":      job.ID,
				"reason":      "not_current_lease_owner",
				"worker_id":   req.WorkerID,
				"lease_owner": job.LeaseOwner,
			})
			w.WriteHeader(http.StatusConflict)
			return
		}

		if req.LeaseID != job.LeaseID { //if the leased ids don't match
			logEvent("ack_rejected", map[string]interface{}{
				"job_id":           job.ID,
				"reason":           "stale_lease_id",
				"worker_lease_id":  req.LeaseID,
				"current_lease_id": job.LeaseID,
			})
			w.WriteHeader(http.StatusConflict)
			return
		}

		if job.LeaseExpiresAt <= time.Now().Unix() { //if the job has already expired we can't let a worker /ack it
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

		// Must be leased to this worker and has to be in the leased state
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
