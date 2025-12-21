# Distributed Task Queue in Go

## Overview
This project is a **Distributed Task Queue** implemented in **Go** as part of my journey to learn **Distributed Systems** and **Go** through project-based learning. The goal of this project is to build a foundational understanding of distributed systems concepts such as **task queues**, **workers**, **leases**, **dead letter queues (DLQ)**, and **retry mechanisms** while applying Go's concurrency features like **goroutines**, **mutexes**, and **channels**.

The task queue is designed to simulate a distributed system where producers enqueue jobs, workers process them, and the system handles retries, failures, and lease expirations. This project is a learning exercise and is not intended for production use.

For a day-by-day build log and what I learned, see DEVLOG.md.

---

## Features Implemented

### 1. **Job Enqueuing**
- Producers can enqueue jobs via the `/enqueue` endpoint.
- Each job has:
  - A unique ID.
  - A payload (arbitrary data).
  - A state (`QUEUED`, `LEASED`, `DONE`, or `DEAD`).
  - Retry bookkeeping (e.g., `Attempts`, `MaxTries`).

### 2. **Job Polling**
- Workers can poll for jobs via the `/poll` endpoint.
- Jobs in the `QUEUED` state are leased to workers for a fixed duration (30 seconds).
- Leased jobs are assigned to a worker (`LeaseOwner`) and have a `LeaseExpiresAt` timestamp.

### 3. **Acknowledging Job Completion**
- Workers can acknowledge job completion via the `/ack` endpoint.
- If the job is successfully processed, its state is updated to `DONE`.

### 4. **Handling Job Failures**
- Workers can report job failures via the `/fail` endpoint.
- Failed jobs are retried with **exponential backoff and jitter**:
  - The retry delay increases exponentially with the number of attempts.
  - Jitter adds randomness to the delay to prevent synchronized retries (thundering herd problem).
- Jobs that exceed the maximum retry attempts (`MaxTries`) are marked as `DEAD` and moved to the **Dead Letter Queue (DLQ)**.

### 5. **Dead Letter Queue (DLQ)**
- Jobs that fail repeatedly are moved to the DLQ.
- The `/dead` endpoint allows inspection of jobs in the DLQ for debugging or manual intervention.

### 6. **Lease Expiration**
- If a worker fails to complete a job within the lease duration, the lease expires.
- Expired jobs are returned to the `QUEUED` state, making them available for other workers to process.

### 7. **Health Check**
- A simple `/health` endpoint is available to verify that the server is running.

---

## Technical Highlights

### **Concurrency**
- The task queue uses **goroutines** and a **time.Ticker** to periodically check for expired leases.
- A **mutex (`sync.Mutex`)** ensures thread-safe access to the shared `jobs` map.

### **Retry Mechanism**
- **Exponential Backoff with Jitter**:
  - Delays between retries grow exponentially: \( 2^{\text{attempts} - 1} \).
  - Jitter introduces randomness to avoid synchronized retries.

### **Dead Letter Queue**
- Jobs that exceed the maximum retry attempts are marked as `DEAD` and can be inspected via the `/dead` endpoint.

### **HTTP Endpoints**
- **`/enqueue`**: Add a new job to the queue.
- **`/poll`**: Workers poll for jobs to process.
- **`/ack`**: Workers acknowledge successful job completion.
- **`/fail`**: Workers report job failures.
- **`/dead`**: Inspect jobs in the Dead Letter Queue.
- **`/health`**: Check server health.

---

## Example Workflow

1. **Enqueue a Job**:
   - Send a POST request to `/enqueue` with a payload.
   - The job is added to the queue in the `QUEUED` state.

2. **Poll for a Job**:
   - A worker sends a POST request to `/poll` with its `WorkerID`.
   - The server leases a job to the worker, updating its state to `LEASED`.

3. **Acknowledge or Fail the Job**:
   - If the worker completes the job, it sends a POST request to `/ack`.
   - If the worker fails to process the job, it sends a POST request to `/fail`.
   - Failed jobs are retried with exponential backoff and jitter.

4. **Handle Dead Jobs**:
   - Jobs that exceed the maximum retry attempts are moved to the DLQ.
   - Inspect dead jobs via the `/dead` endpoint.

5. **Lease Expiration**:
   - If a worker does not complete a job within the lease duration, the lease expires.
   - The job is returned to the `QUEUED` state for other workers to process.

---

## Future Enhancements
- **Persistent Storage**: Replace the in-memory `jobs` map with a database to ensure durability.
- **Worker Heartbeats**: Add a mechanism for workers to send periodic heartbeats to extend leases.
- **Metrics and Monitoring**: Add metrics for job processing, retries, and failures.
- **Support for Priority Queues**: Allow jobs to be processed based on priority levels.

---

## Why I Built This
I wanted to gain a deeper understanding of distributed systems concepts and Go's concurrency model. By building this project, I learned how to:
- Design and implement a task queue.
- Handle concurrency and synchronization in Go.
- Implement retry mechanisms with exponential backoff and jitter.
- Manage job states and handle failures gracefully.

This project is a stepping stone in my journey to mastering distributed systems and Go.

---

## How to Run
1. Install Go (if not already installed): [Download Go](https://go.dev/dl/).
2. Clone this repository:
   ```bash
   git clone <repository-url>
   cd ds-task-queue
   ```
3. Run the server:
   ```bash
   go run .
   ```
4. Use tools like `curl` or Postman to interact with the HTTP endpoints.

---

## Endpoints Summary

| **Endpoint**   | **Method** | **Description**                          |
|-----------------|------------|------------------------------------------|
| `/enqueue`      | POST       | Add a new job to the queue.              |
| `/poll`         | POST       | Workers poll for jobs to process.        |
| `/ack`          | POST       | Acknowledge successful job completion.   |
| `/fail`         | POST       | Report job failure and trigger retries.  |
| `/dead`         | GET        | Inspect jobs in the Dead Letter Queue.   |
| `/jobs`         | GET        | View all jobs in the system.             |
| `/health`       | GET        | Check if the server is running.          |

---

## License
This project is for educational purposes and is not intended for production use. Feel free to use it as a reference or learning resource.