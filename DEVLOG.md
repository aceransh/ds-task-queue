# Distributed Task Queue — Development Log

This document tracks the day-by-day development of a distributed task queue built to learn core distributed systems concepts through implementation and failure testing.

The goal is not to build a production-ready system, but to understand *why* real systems are designed the way they are.

---

## Day 1 — Foundations & Distributed Systems Mindset

### Concepts Learned

- Distributed systems fail in **partial** and **unpredictable** ways
- Timeouts ≠ failures
- At-most-once vs at-least-once delivery
- Why duplicates are unavoidable in distributed systems
- State machines as the foundation of correctness

### What I Built

- Basic Go project structure
- HTTP server using `net/http`
- `/health` endpoint for liveness checks
- `/enqueue` endpoint that accepts JSON payloads
- `Job` struct and explicit job states (`QUEUED`, `LEASED`, `DONE`, `DEAD`)

### Key Takeaway

I learned early that correctness in distributed systems comes from **explicit state transitions**, not assumptions about timing or reliability. Even the simplest queue must assume retries, duplicates, and crashes.

---

## Day 2 — In-Memory Job Store & Concurrency

### Concepts Learned

- Shared mutable state is dangerous without synchronization
- Go maps are **not** safe for concurrent access
- Mutexes are required even for concurrent reads
- Race conditions can exist even in single-node systems

### What I Built

- In-memory job store using `map[string]*Job`
- UUID-based job IDs
- Mutex-protected access to the job map
- `/jobs` debug endpoint to inspect internal state
- `/enqueue` now creates and stores jobs

### Failure Testing

- Concurrent enqueues
- Concurrent reads of `/jobs`
- Verified that missing locks cause crashes

### Key Takeaway

Distributed systems problems appear **even before distribution**. Concurrency bugs are just as real on a single node, and correctness starts with disciplined state access.

---

## Day 3 — Worker Polling, Leasing, and Acknowledgement Safety

### Concepts Learned

- Removing jobs immediately is unsafe
- Workers can crash after receiving work
- Leasing is required for liveness
- Liveness is more important than uniqueness
- A job being “taken” does not mean it is “done”

### What I Built

- `/poll` endpoint for workers to request jobs
- Lease-based job assignment (`LEASED` state)
- `LeaseOwner` and `LeaseExpiresAt` fields
- Background lease expiration logic
- `/ack` endpoint for workers to mark jobs as completed
- Validation to prevent:
  - Acks from the wrong worker
  - Acks after lease expiration
  - State transitions from invalid states

### Failure Testing

- Worker crash simulation (poll without ack)
- Lease expiration causing job to be re-queued
- Duplicate polls by multiple workers
- Verified that stale acks are rejected

### Key Takeaway

Leases are the foundation of fault tolerance in task queues. This system intentionally allows duplicate execution in exchange for guaranteed progress (at-least-once delivery).

---

## Day 4 — Retries, Backoff, and Dead Letter Queue (DLQ)

### Concepts Learned

- Retry storms can overload systems
- Immediate retries are dangerous
- Exponential backoff prevents cascading failures
- Full jitter prevents thundering herd problems
- Poison messages must be isolated (DLQ)

### What I Built

- Retry tracking (`Attempts`, `MaxTries`)
- `NextAvailableAt` field to delay retries
- `/fail` endpoint for workers to report failures
- Exponential backoff with **full jitter**
- Jobs transition to `DEAD` after exceeding max retries
- `/dead` endpoint to inspect dead-lettered jobs
- Lease expiration logic clears retry delays appropriately

### Failure Testing

- Repeated job failures trigger backoff
- Jobs are not re-polled until delay expires
- Jobs move to `DEAD` after max retries
- Verified DLQ visibility via `/dead`

### Key Takeaway

Retries must be **controlled**, not automatic. Backoff and DLQs are not optimizations — they are required for system stability under failure.

---

## Day 5 — Idempotency & Exactly-Once Effects

### Concepts Learned

- Client retries are unavoidable due to timeouts and lost responses
- Exactly-once *delivery* is unrealistic in distributed systems
- Exactly-once *effects* can be achieved through idempotency
- Idempotency requires atomic “check-and-claim” semantics
- Concurrency can break idempotency without proper coordination

### What I Built

- Idempotent job creation using the `Idempotency-Key` request header
- Mapping from idempotency key → job ID to deduplicate retries
- Atomic reservation of idempotency keys using a `"PENDING"` marker
- `409 Conflict` response when a duplicate request arrives while the original is still in progress
- Safe cleanup of `"PENDING"` state on request failure
- Retried requests return the original `job_id` without creating new jobs

### Semantics & Guarantees

- Repeated `/enqueue` requests with the same idempotency key return the **same `job_id`**
- No duplicate jobs are created for the same logical client request
- Concurrent duplicate requests are rejected with `409 Conflict`
- The system provides **exactly-once effects for job creation**, not exactly-once execution

**Contract:** Clients must not reuse an idempotency key for different logical requests. If reused with a different payload, the broker may still return the original `job_id` (lenient behavior).

### Failure Testing

- Retried `/enqueue` after response loss returns same `job_id`
- Concurrent `/enqueue` requests with the same key result in:
  - one successful job creation
  - one or more `409 Conflict` responses
- Verified that race conditions do not create duplicate jobs
- Verified cleanup of idempotency state on malformed requests

### Key Takeaway

Distributed systems do not eliminate duplicates — they make duplicates **safe**.  
By combining idempotency keys with atomic reservation, the system achieves exactly-once *effects* while preserving at-least-once delivery semantics.