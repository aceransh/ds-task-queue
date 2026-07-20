# Distributed Task Queue (Go)

## Overview

This project is a **Distributed Task Queue** implemented in **Go**, built to deeply understand core **distributed systems fundamentals** through hands-on implementation and failure testing[cite: 3]. 

The system models a real-world task queue (SQS / Celery–style) where:
- producers enqueue jobs,[cite: 3]
- workers poll and process them,[cite: 3]
- and the system remains correct under crashes, retries, and partial failures[cite: 3].

The focus is on **correctness under failure**, not performance or production readiness[cite: 3]. 

---

## System Guarantees & Semantics

### Delivery
- **At-least-once delivery:** Jobs may be delivered more than once[cite: 3]. This is intentional and required for fault tolerance[cite: 3].

### Exactly-once Effects (Enqueue)
- **Idempotent enqueue via `Idempotency-Key`**[cite: 3]
  - Repeated `/enqueue` requests with the same idempotency key return the same `job_id`[cite: 3].
  - Duplicate jobs are not created for the same logical request[cite: 3].
  - Concurrent duplicate requests return `409 Conflict` while the original request is in progress[cite: 3].

> The system does **not** guarantee exactly-once execution[cite: 3]. Instead, it guarantees **exactly-once effects** for job creation[cite: 3].

### Leasing & Liveness
- Jobs are leased to workers for a fixed duration (30 seconds)[cite: 3].
- If a worker crashes or stalls, the lease expires and the job becomes visible again[cite: 3].
- Liveness is prioritized over uniqueness[cite: 3].

### Retries & Dead Letter Queue (DLQ)
- Failed jobs retry with **exponential backoff and full jitter** to prevent retry storms and thundering herd effects[cite: 3].
- Jobs exceeding `MaxTries` are moved to a **Dead Letter Queue (DLQ)**[cite: 3].
- Poison messages are isolated instead of retried indefinitely[cite: 3].
- Dead jobs can be inspected via `/dead` for debugging or manual intervention[cite: 3].

---

## Architecture Overview

### Core Components
- **Broker (Go):** owns job state, leasing, retries, and failure handling[cite: 3].
- **Database (PostgreSQL):** highly concurrent state management utilizing `FOR UPDATE SKIP LOCKED` to prevent worker contention.
- **Workers:** poll for jobs, process them, and acknowledge success or failure[cite: 3].
- **Lease Sweeper:** background goroutine that periodically re-queues expired leases[cite: 3].
- **Observability Stack:** Prometheus and Grafana for real-time metric scraping (Pull Model).

---

## Implemented Features

### 1. Advanced Job Routing & Polling
- **Long Polling (`/poll`):** Workers may long-poll for jobs for up to **30 seconds**[cite: 3]. If a job is immediately available, it is returned right away[cite: 3]. If no jobs are available, the request blocks and returns `204 No Content` after the timeout[cite: 3]. If a job becomes available while polling, the request returns immediately with a leased job[cite: 3]. This reduces unnecessary polling traffic and mirrors production queue behavior (e.g., AWS SQS long polling)[cite: 3].
- **Job Acknowledgement (`/ack`):** Workers explicitly acknowledge successful completion[cite: 3]. Stale or invalid acknowledgements are rejected[cite: 3].
- **Job Failure Handling (`/fail`):** Workers report failed processing attempts[cite: 3]. Retries scheduled using exponential backoff + jitter[cite: 3].

### 2. Production-Grade Resiliency
- **Graceful Shutdown:** The broker intercepts OS signals (`SIGINT`, `SIGTERM`) to halt incoming requests, flush active database transactions, and close connection pools before exiting.
- **Database Concurrency:** Job leasing is protected by transactional row-level locking (`FOR UPDATE SKIP LOCKED`), ensuring horizontal scalability without duplicate processing overhead.

### 3. Observability
- Exposes a `/metrics` endpoint for Prometheus.
- Tracks `task_queue_jobs_enqueued_total`, `task_queue_jobs_acked_total`, and `task_queue_jobs_failed_total`.

---

## HTTP API Summary

| Endpoint | Method | Description |
|--------|--------|-------------|
| `/enqueue` | POST | Enqueue a job (idempotent)[cite: 3]|
| `/poll` | POST | Poll for a job (supports long polling)[cite: 3]|
| `/ack` | POST | Acknowledge successful job[cite: 3]|
| `/fail` | POST | Report job failure[cite: 3]|
| `/dead` | GET | Inspect dead-lettered jobs[cite: 3]|
| `/jobs` | GET | Inspect all jobs (debug)[cite: 3]|
| `/health` | GET | Health check[cite: 3]|
| `/metrics` | GET | Prometheus scrape endpoint |

---

## Failure Harness

The system includes a lightweight failure harness used to validate correctness under real failure conditions[cite: 3]. The harness simulates:
- high-rate job enqueuing[cite: 3]
- multiple concurrent workers[cite: 3]
- worker crashes and stalls[cite: 3]
- broker restarts[cite: 3]

### Failure Scenarios Tested
- Worker crashes mid-processing[cite: 3]
- Worker stalls beyond lease duration[cite: 3]
- Database connection loss and automatic retry
- Duplicate enqueue requests (idempotency)[cite: 3]
- Retry storms and poison messages[cite: 3]
- Stale acknowledgements[cite: 3]
- Concurrent workers contending for jobs[cite: 3]

All correctness guarantees are validated through **observable state transitions** rather than mocks or unit tests[cite: 3].

---

## Why This Project

This project was built to:
- Understand **why** distributed systems are designed the way they are[cite: 3].
- Learn Go through real concurrency problems[cite: 3].
- Demonstrate understanding of **network-efficient queue design** via long polling[cite: 3].
- Serve as a foundational portfolio piece targeting backend engineering and AI SRE roles in the San Diego tech market.

It intentionally trades completeness for clarity and correctness[cite: 3].

---

## How to Run

```bash
docker-compose up --build

```

* **Broker API:** `http://localhost:8080`
* **Prometheus UI:** `http://localhost:9090`
* **Grafana Dashboards:** `http://localhost:3000`

Use `curl` or Postman to interact with the API, or run the included `enqueue_spam.sh` and `worker.sh` bash scripts to simulate load.

---

## License

Educational use only. Not intended for production deployment.