package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ds-task-queue/internal/models"
	"github.com/google/uuid"
)

var brokerURL = getEnv("BROKER_URL", "http://localhost:8080")

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func pollForJob(ctx context.Context, client *http.Client, workerID string) (*models.Job, error) {
	reqBody := models.PollRequest{WorkerID: workerID}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed marshal req: %v", err)
	}

	pollUrl := fmt.Sprintf("%s/poll", brokerURL)

	// 2. built a custom request and strapped Context to it
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pollUrl, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 3. executed the custom request using client.Do()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error during poll: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil //queue is empty
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("broker returned unexpected status: %d", resp.StatusCode)
	}

	var job models.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, fmt.Errorf("failed to decode job: %v", err)
	}

	return &job, nil
}

func ackJob(ctx context.Context, client *http.Client, workerID string, jobID string, leaseID int64) error {
	reqBody := models.AckRequest{WorkerID: workerID, JobID: jobID, LeaseID: leaseID}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed marshall req: %v", err)
	}

	ackUrl := fmt.Sprintf("%s/ack", brokerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ackUrl, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("network error during ack: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker returned unexpected status: %d", resp.StatusCode)
	}

	return nil
}

func failJob(ctx context.Context, client *http.Client, workerID string, jobID string, leaseID int64) error {
	reqBody := models.FailRequest{WorkerID: workerID, JobID: jobID, LeaseID: leaseID}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed marshall req: %v", err)
	}

	failUrl := fmt.Sprintf("%s/fail", brokerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, failUrl, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("network error during fail: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker returned unexpected status: %d", resp.StatusCode)
	}

	return nil
}

func main() {
	workerID := uuid.NewString()
	log.Printf("Starting Task Queue Worker | ID: %s", workerID)

	// Create a Context that listens for Ctrl+C (SIGINT) or termination commands (SIGTERM)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop() // This cleans up the signal listener when main() finishes

	client := &http.Client{
		Timeout: 35 * time.Second,
	}

	for {
		// 1. Check if the OS hit the self-destruct button before we poll
		select {
		case <-ctx.Done():
			log.Println("Graceful shutdown initiated. Worker exiting.")
			return
		default:
			// If not cancelled, fall through and continue loop
		}

		// 2. Pass the Context into the poll function
		job, err := pollForJob(ctx, client, workerID)
		if err != nil {
			// If the context is canceled during a network request, it throws a context.Canceled error
			if ctx.Err() != nil {
				log.Println("Poll interrupted by shutdown signal.")
				return
			}
			log.Printf("Worker ID: %s had an error polling for a job: %v", workerID, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if job == nil {
			continue
		}

		log.Printf("Acquired Job ID: %s | Executing payload: %s", job.ID, job.Payload)
		time.Sleep(3 * time.Second) //sim worker doing task

		// 3. We use context.Background() for the ACK.
		// WHY? Because if the user hits Ctrl+C while the worker is doing a job, we DO NOT want to cancel the ACK request!
		err = ackJob(context.Background(), client, workerID, job.ID, job.LeaseID)
		if err != nil {
			log.Printf("Critical: Failed to ack Job ID: %s | Error: %v", job.ID, err)
		} else {
			log.Printf("Successfully finished and acked Job ID: %s", job.ID)
		}
	}
}
