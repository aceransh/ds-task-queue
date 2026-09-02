package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ds-task-queue/internal/db"
	"ds-task-queue/internal/models"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	jobsEnqueuedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_queue_jobs_enqueued_total",
		Help: "Total number of jobs succesfully enqueued",
	})
	jobsAckedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_queue_jobs_acked_total",
		Help: "Total number of jobs succesfully acknowledged",
	})
	jobsFailedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_queue_jobs_failed_total",
		Help: "Total number of job fails recorded",
	})
)

type Server struct {
	db      *sql.DB
	jobsMu  sync.Mutex
	jobCond *sync.Cond
}

func NewServer(db *sql.DB) *Server {
	server := &Server{db: db}
	server.jobCond = sync.NewCond(&server.jobsMu)
	return server
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		// 1. Try to lock the key by inserting PENDING
		result, err := s.db.Exec(`
				INSERT INTO Idems (idem_key, status)
				VALUES ($1, 'PENDING')
				ON CONFLICT (idem_key) DO NOTHING;
			`, idemKey) //since we do on conflict do nothing it means if it already exists or it is already pointing to jobId no error will throw and just move on.
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

			err := s.db.QueryRow("SELECT status, job_id FROM Idems WHERE idem_key = $1", idemKey).Scan(&status, &existingJobID)
			if err != nil {
				log.Println("Query idempotency error:", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if status == "PENDING" { //job is already currently being made which is why its pending
				w.WriteHeader(http.StatusConflict)
				return
			}

			// It must be COMPLETED, so return the existing job ID
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"job_id":"%s"}`, existingJobID.String)
			return
		}
	}

	var req models.EnqueueRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		log.Println("Request error: ", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var id string = uuid.NewString()

	tx, err := s.db.Begin()
	if err != nil {
		log.Println("Error with db.DB.Begin(): ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec("INSERT INTO Jobs (id, payload, state) VALUES ($1, $2, $3)", id, req.Payload, models.StateQueued)
	if err != nil {
		log.Println("Failed to insert job: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// return triggers defer tx.Rollback() if it fails
	if idemKey != "" {
		_, err = tx.Exec("UPDATE Idems SET status = 'COMPLETED', job_id = $1 WHERE idem_key = $2", id, idemKey)
		if err != nil {
			log.Println("Failed to update idem_key: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Println("Failed to commit tx: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	s.jobsMu.Lock()
	s.jobCond.Signal()
	s.jobsMu.Unlock()

	jobsEnqueuedMetric.Inc()

	logEvent("job_enqueued", map[string]interface{}{
		"job_id":          id,
		"payload_len":     len(req.Payload),
		"idempotency_key": idemKey,
	})

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"job_id":"%s"}`, id)
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req models.PollRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil || req.WorkerID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	timeout := 30 * time.Second
	deadline := time.Now().Add(timeout)

	for {
		now := time.Now().Unix()
		var job models.Job

		err := s.db.QueryRow(`
				UPDATE Jobs
				SET state = 'LEASED', lease_owner = $1, lease_expires_at = $2, lease_id = lease_id + 1
				WHERE id = (
					SELECT id from Jobs
					WHERE state = 'QUEUED' AND (next_available_at = 0 OR next_available_at <= $3)
					FOR UPDATE SKIP LOCKED LIMIT 1
				) RETURNING id, payload, attempts, max_tries, lease_id, state, lease_owner, lease_expires_at;
			`, req.WorkerID, now+30, now).Scan(&job.ID, &job.Payload, &job.Attempts, &job.MaxTries, &job.LeaseID, &job.State, &job.LeaseOwner, &job.LeaseExpiresAt)
		//this basically just takes the first available job that isn't being looked at by another worker and set it to leased and gives it all its fields lol

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

		case sql.ErrNoRows:
			//when no job available
			remaining := time.Until(deadline)
			if remaining <= 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			timer := time.AfterFunc(remaining, func() {
				s.jobsMu.Lock()
				s.jobCond.Signal()
				s.jobsMu.Unlock()
			})

			s.jobsMu.Lock()
			s.jobCond.Wait()
			s.jobsMu.Unlock()

			timer.Stop()

		default:
			log.Println("Failed to poll for job: ", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req models.AckRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil || req.WorkerID == "" || req.JobID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := s.db.Begin() //setup a transaction
	if err != nil {
		log.Println("Error with db.Begin(): ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var job models.Job

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

	if job.State == models.StateDone { //already been acked
		w.WriteHeader(http.StatusOK)
		return
	}

	if job.State != models.StateLeased || job.LeaseOwner != req.WorkerID { //if either the state isn't leased or the leaseowner doesn't match the worker id from the request
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
		`, models.StateDone, req.JobID)
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

	jobsAckedMetric.Inc()

	logEvent("job_acked", map[string]interface{}{
		"job_id":    job.ID,
		"worker_id": req.WorkerID,
		"lease_id":  req.LeaseID,
	})

	w.WriteHeader(http.StatusOK)

}

func (s *Server) handleFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req models.FailRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil || req.WorkerID == "" || req.JobID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := s.db.Begin() //setup a transaction
	if err != nil {
		log.Println("Error with db.Begin(): ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var job models.Job

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

	if job.State == models.StateDone {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Must be leased to this worker and has to be in the leased state
	if job.State != models.StateLeased || job.LeaseOwner != req.WorkerID {
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
		job.State = models.StateDead
		job.LeaseOwner = ""
		job.LeaseExpiresAt = 0
		job.NextAvailableAt = 0

		logEvent("job_dead", map[string]interface{}{
			"job_id":   job.ID,
			"attempts": job.Attempts,
		})

	} else {

		// Retry later with backoff + full jitter
		delay := retryDelaySeconds(job.Attempts)

		job.State = models.StateQueued
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

	jobsFailedMetric.Inc()

	w.WriteHeader(http.StatusOK)

}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rows, err := s.db.Query(`
        SELECT id, payload, state, attempts, max_tries, lease_id,
                COALESCE(lease_owner, ''), COALESCE(lease_expires_at, 0), COALESCE(next_available_at, 0)
            FROM Jobs`)
	if err != nil {
		log.Println("Failed to query: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	jobs := make(map[string]*models.Job)

	for rows.Next() {
		var job models.Job

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
}

func (s *Server) handleDead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rows, err := s.db.Query(`
            SELECT id, payload, state, attempts, max_tries, lease_id,
                COALESCE(lease_owner, ''), COALESCE(lease_expires_at, 0), COALESCE(next_available_at, 0)
            FROM Jobs
            WHERE state = $1
        `, models.StateDead)
	if err != nil {
		log.Println("Failed to query: ", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	dead := make(map[string]*models.Job)

	for rows.Next() {
		var job models.Job

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
}

func (s *Server) Sweep() []uuid.UUID {
	now := time.Now().Unix()

	var expiredIDs []uuid.UUID = make([]uuid.UUID, 0)

	tx, err := s.db.Begin()
	if err != nil {
		log.Println("Error with db.DB.Begin(): ", err)
		return expiredIDs
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		UPDATE Jobs
		SET state=$1, lease_owner=NULL, lease_expires_at=0, next_available_at=0
		WHERE id IN (
			SELECT id from Jobs WHERE state = 'LEASED' AND lease_expires_at <= $2
			FOR UPDATE SKIP LOCKED
		) RETURNING id`, models.StateQueued, now)
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
	}

	if len(expiredIDs) > 0 {
		s.jobsMu.Lock()
		s.jobCond.Signal()
		s.jobsMu.Unlock()
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

func logEvent(event string, fields map[string]any) { //formats logs so that its space seperated and always starts with what event happened
	msg := fmt.Sprintf("event=%s", event)
	for k, v := range fields {
		msg += fmt.Sprintf(" %s=%v", k, v)
	}
	log.Println(msg)
}

func init() {
	prometheus.MustRegister(jobsEnqueuedMetric)
	prometheus.MustRegister(jobsAckedMetric)
	prometheus.MustRegister(jobsFailedMetric)
}

func main() {
	db.InitDB()
	srv := NewServer(db.DB)

	http.HandleFunc("/health", srv.handleHealth)

	http.Handle("/metrics", promhttp.Handler())

	http.HandleFunc("/enqueue", srv.handleEnqueue)

	http.HandleFunc("/poll", srv.handlePoll)

	http.HandleFunc("/ack", srv.handleAck)

	http.HandleFunc("/fail", srv.handleFail)

	http.HandleFunc("/jobs", srv.handleJobs)

	http.HandleFunc("/dead", srv.handleDead)

	// --- BACKGROUND PROCESSES ---

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			srv.Sweep()
		}
	}()

	// --- GRACEFUL SHUTDOWN SERVER LOGIC ---

	server := &http.Server{
		Addr:    ":8080",
		Handler: nil,
	}

	go func() {
		log.Println("Listening on port 8080")

		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Fatal("Server Crashed: ", err)
		}
	}()

	quit := make(chan os.Signal, 1)

	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	<-quit

	log.Println("Interrupt signal received. Initiating graceful shutdown...")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Closing database connections...")
	if err := db.DB.Close(); err != nil {
		log.Fatalf("Database connection close failed: %v", err)
	}

	log.Println("Graceful shutdown complete.")
}
