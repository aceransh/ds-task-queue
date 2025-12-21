# Distributed Task Queue (Go)

## Overview

This project is a **Distributed Task Queue** implemented in **Go**, built to deeply understand core **distributed systems fundamentals** through hands-on implementation and failure testing.

The system models a real-world task queue (SQS / Celery–style) where:

- producers enqueue jobs,
- workers poll and process them,
- and the system remains correct under crashes, retries, and partial failures.

The focus is on **correctness under failure**, not performance or production readiness.

For a day-by-day build log and design reasoning, see **DEVLOG.md**.

---

## System Guarantees & Semantics

### Delivery

- **At-least-once delivery**  
  Jobs may be delivered more than once. This is intentional and required for fault tolerance.

### Exactly-once Effects (Enqueue)

- **Idempotent enqueue via `Idempotency-Key`**
  - Repeated `/enqueue` requests with the same idempotency key return the same `job_id`.
  - Duplicate jobs are not created for the same logical request.
  - Concurrent duplicate requests return `409 Conflict` while the original request is in progress.

> The system does **not** guarantee exactly-once execution. Instead, it guarantees **exactly-once effects** for job creation.

### Leasing & Liveness

- Jobs are leased to workers for a fixed duration (30 seconds).
- If a worker crashes or stalls, the lease expires and the job becomes visible again.
- Liveness is prioritized over uniqueness.

### Retries

- Failed jobs retry with **exponential backoff and full jitter** to prevent retry storms and thundering herd effects.
- Jobs exceeding `MaxTries` are moved to a **Dead Letter Queue (DLQ)**.

### Dead Letter Queue (DLQ)

- Poison messages are isolated instead of retried indefinitely.
- Dead jobs can be inspected via `/dead` for debugging or manual intervention.

### Limitations (Explicit)

- In-memory storage only — broker restarts lose state.
- Single-node broker (distribution and replication are future work).
- Workers must be idempotent to safely handle duplicate execution.

---

## Architecture Overview

### Job States

QUEUED → LEASED → DONE
↘
DEAD

### Core Components

- **Broker**: owns job state, leasing, retries, and failure handling
- **Workers**: poll for jobs, process them, and acknowledge success or failure
- **Lease Sweeper**: periodically re-queues expired leases

---

## Implemented Features

### Job Enqueuing (`/enqueue`)

- Creates new jobs in the `QUEUED` state
- Supports idempotent creation via `Idempotency-Key`
- Returns a stable `job_id` for retries

### Worker Polling (`/poll`)

- Workers request jobs to process
- Jobs are leased (not removed) to tolerate crashes
- Only jobs eligible by retry delay are returned

### Job Acknowledgement (`/ack`)

- Workers explicitly acknowledge successful completion
- Stale or invalid acknowledgements are rejected

### Job Failure Handling (`/fail`)

- Workers report failed processing attempts
- Retries scheduled using exponential backoff + jitter
- Jobs transition to `DEAD` after exceeding retry limit

### Dead Letter Queue (`/dead`)

- Lists jobs that permanently failed
- Provides observability into poison messages

### Lease Expiration

- Background goroutine reclaims expired leases
- Prevents job loss due to crashed or slow workers

### Health Check (`/health`)

- Simple liveness endpoint for monitoring

---

## Concurrency Model

- Shared state protected by `sync.Mutex`
- Explicit state transitions enforce correctness
- Concurrency issues are treated as first-class failure modes

---

## HTTP API Summary

| Endpoint | Method | Description |
|--------|--------|-------------|
| `/enqueue` | POST | Enqueue a job (idempotent) |
| `/poll` | POST | Poll for a job to process |
| `/ack` | POST | Acknowledge successful job |
| `/fail` | POST | Report job failure |
| `/dead` | GET | Inspect dead-lettered jobs |
| `/jobs` | GET | Inspect all jobs (debug) |
| `/health` | GET | Health check |

---

## Failure Scenarios Tested

- Worker crashes mid-processing
- Worker stalls beyond lease duration
- Duplicate enqueue requests
- Retry storms and poison messages
- Stale acknowledgements
- Concurrent access to shared state

---

## Why This Project

This project was built to:

- Understand **why** distributed systems are designed the way they are
- Learn Go through real concurrency problems
- Build something that can be confidently discussed in interviews

It intentionally trades completeness for clarity and correctness.

---

## How to Run

```bash
go run .

Use curl or Postman to interact with the API.
```

⸻

## License

Educational use only. Not intended for production deployment.

---

## Why this version is better

- Clearly separates **guarantees vs limitations**
- Uses correct distributed systems language
- Highlights idempotency, leases, retries, and DLQ (your strongest work)
- Reads like a **systems design artifact**, not a tutorial

If you want, next we can:

- tighten it even more for **resume bullets**
- add an **Architecture Diagram** section
- or start **Day 6: fencing tokens / zombie worker protection**

Just tell me what you want to tackle next.
