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
	jobsMu sync.Mutex

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

func expireLeases(now int64) []uuid.UUID {
	var expiredIDs []uuid.UUID = make([]uuid.UUID, 0)

	tx, err := db.Begin()
	if err != nil {
		log.Println("Error with db.Begin(): ", err)
		return expiredIDs
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
	UPDATE Jobs
	SET state=$1, lease_owner=NULL, lease_expires_at=0, next_available_at=0
	WHERE id IN (
				SELECT id from Jobs
				WHERE state = 'LEASED'
					AND lease_expires_at <= $2
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id`, StateQueued, now)
	if err != nil {
		log.Println("Error with tx.Query(): ", err)
		return expiredIDs
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID

		err = rows.Scan(&id)
		if err != nil {
			log.Println("Error with rows.Scan(): ", err)
			return expiredIDs
		}

		expiredIDs = append(expiredIDs, id)
	}

	if err = rows.Err(); err != nil {
		log.Println("Error with iteration: ", err)
		return expiredIDs
	}

	if err = tx.Commit(); err != nil {
		log.Println("Error committing lease expiration tx:", err)
		return expiredIDs
	}

	for _, id := range expiredIDs {
		logEvent("lease_expired", map[string]interface{}{
			"job_id": id,
		})

		jobsMu.Lock()
		jobCond.Signal()
		jobsMu.Unlock()
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
				INSERT INTO Idems (idem_key, status) 
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

				err := db.QueryRow("SELECT status, job_id FROM Idems WHERE idem_key = $1", idemKey).Scan(&status, &existingJobID)
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

		tx, err := db.Begin() //setup a transaction
		if err != nil {
			log.Println("Error with db.Begin(): ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var job Job

		err = tx.QueryRow(`
		SELECT id, state, COALESCE(lease_owner, ''), lease_id, COALESCE(lease_expires_at, 0) 
		FROM jobs 
		WHERE id = $1 FOR UPDATE
		`, req.JobID).Scan(&job.ID, &job.State, &job.LeaseOwner, &job.LeaseID, &job.LeaseExpiresAt)
		if err != nil {
			if err == sql.ErrNoRows {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			log.Println("Error with tx query: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// job, ok := jobs[req.JobID]
		// if !ok {
		// 	w.WriteHeader(http.StatusNotFound)
		// 	return
		// }

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
		_, err = tx.Exec(`
		UPDATE Jobs
		SET state=$1, lease_owner='', lease_expires_at=0
		WHERE id=$2
		`, StateDone, req.JobID)
		if err != nil {
			log.Println("Failed to update job fields: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return // This triggers defer tx.Rollback()
		}

		err = tx.Commit()
		if err != nil {
			log.Println("Failed to commit tx: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return // This triggers defer tx.Rollback()
		}

		// job.State = StateDone
		// job.LeaseOwner = ""
		// job.LeaseExpiresAt = 0

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

		tx, err := db.Begin() //setup a transaction
		if err != nil {
			log.Println("Error with db.Begin(): ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var job Job

		err = tx.QueryRow(`
		SELECT id, state, COALESCE(lease_owner, ''), lease_id, COALESCE(lease_expires_at, 0), max_tries, attempts 
		FROM jobs 
		WHERE id = $1 FOR UPDATE
		`, req.JobID).Scan(&job.ID, &job.State, &job.LeaseOwner, &job.LeaseID, &job.LeaseExpiresAt, &job.MaxTries, &job.Attempts)
		if err != nil {
			if err == sql.ErrNoRows {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			log.Println("Error with tx query: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// job, ok := jobs[req.JobID]
		// if !ok {
		// 	w.WriteHeader(http.StatusNotFound)
		// 	return
		// }

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
		} else {

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
		}
		_, err = tx.Exec(`
		Update Jobs SET state=$1, lease_owner=$2,
		lease_expires_at=$3, next_available_at=$4, attempts=$5
		WHERE id=$6`, job.State, job.LeaseOwner, job.LeaseExpiresAt, job.NextAvailableAt, job.Attempts, req.JobID)
		if err != nil {
			log.Println("Failed to update job fields: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return // This triggers defer tx.Rollback()
		}

		err = tx.Commit()
		if err != nil {
			log.Println("Failed to commit tx: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return // This triggers defer tx.Rollback()
		}

		w.WriteHeader(http.StatusOK)

	})

	http.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		rows, err := db.Query(`
		SELECT id, payload, state, attempts, max_tries, lease_id, 
				COALESCE(lease_owner, ''), COALESCE(lease_expires_at, 0), COALESCE(next_available_at, 0)
			FROM Jobs`)
		if err != nil {
			log.Println("Failed to query: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		jobs := make(map[string]*Job)

		for rows.Next() {
			var job Job

			err = rows.Scan(&job.ID, &job.Payload, &job.State, &job.Attempts, &job.MaxTries, &job.LeaseID, &job.LeaseOwner, &job.LeaseExpiresAt, &job.NextAvailableAt)
			if err != nil {
				log.Println("Failed to Scan: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			jobs[job.ID] = &job

		}

		if err = rows.Err(); err != nil {
			log.Println("Error in row iteration: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jobs)
	})

	http.HandleFunc("/dead", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		rows, err := db.Query(`
			SELECT id, payload, state, attempts, max_tries, lease_id, 
				COALESCE(lease_owner, ''), COALESCE(lease_expires_at, 0), COALESCE(next_available_at, 0)
			FROM Jobs
			WHERE state = $1
		`, StateDead)
		if err != nil {
			log.Println("Failed to query: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		dead := make(map[string]*Job)

		for rows.Next() {
			var job Job

			err = rows.Scan(&job.ID, &job.Payload, &job.State, &job.Attempts, &job.MaxTries, &job.LeaseID, &job.LeaseOwner, &job.LeaseExpiresAt, &job.NextAvailableAt)
			if err != nil {
				log.Println("Failed to Scan: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			dead[job.ID] = &job

		}

		if err = rows.Err(); err != nil {
			log.Println("Error in row iteration: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
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
