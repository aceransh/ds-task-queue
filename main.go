package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	jobs   = make(map[string]*Job)
	jobsMu sync.Mutex
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

	// Lease info (used when a worker "owns" it temporarily)
	LeaseOwner     string `json:"lease_owner,omitempty"`
	LeaseExpiresAt int64  `json:"lease_expires_at,omitempty"`

	// Retry bookkeeping (we’ll use these in Week 1)
	Attempts int `json:"attempts"`
	MaxTries int `json:"max_tries"`
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
			expiredIDs = append(expiredIDs, id)
		}
	}

	return expiredIDs
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

		var req EnqueueRequest
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
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

		jobsMu.Lock()
		defer jobsMu.Unlock()

		for _, job := range jobs {
			if job.State == StateQueued {
				job.State = StateLeased
				job.LeaseOwner = req.WorkerID
				job.LeaseExpiresAt = time.Now().Unix() + 30

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(job)
				return
			}
		}

		//when no job available
		w.WriteHeader(http.StatusNoContent)
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
			w.WriteHeader(http.StatusConflict)
			return
		}

		if job.LeaseExpiresAt <= time.Now().Unix() {
			w.WriteHeader(http.StatusConflict)
			return
		}

		//mark done
		job.State = StateDone
		job.LeaseOwner = ""
		job.LeaseExpiresAt = 0

		w.WriteHeader(http.StatusOK)

	})

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			ids := expireLeases(time.Now().Unix())
			if len(ids) > 0 {
				fmt.Println("expired lease: ", ids)
			}
		}
	}()

	log.Println("Listening on port 8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
