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

## Day 6 — Zombie Workers & Fencing Tokens

### Concepts Learned

- Workers can become “zombies” due to pauses, crashes, or slow execution
- Lease expiration alone is not sufficient to prevent stale workers from committing state
- Time-based checks are vulnerable to race conditions
- **Fencing tokens** (monotonically increasing lease versions) are required for correctness
- The newest lease holder must be able to *fence out* all previous holders

### The Zombie Worker Problem

A subtle but critical failure scenario:

1. Worker A leases a job and begins processing
2. Worker A stalls (GC pause, CPU starvation, etc.)
3. The lease expires and the job is re-queued
4. Worker B leases the same job and begins processing
5. Worker A resumes and attempts to ACK

Without additional safeguards, **both workers believe they own the job**.  
Accepting Worker A’s ACK would violate correctness and potentially cause duplicate side effects.

### What I Built

- Added a monotonically increasing `LeaseID` (fencing token) to each job
- Incremented `LeaseID` on every successful `/poll` (new lease)
- Returned `lease_id` to workers as part of the poll response
- Required workers to include `lease_id` on `/ack` and `/fail`
- Rejected ACK/FAIL requests when:
  - the lease ID is stale
  - the worker is not the current lease owner
  - the lease has expired

### Semantics & Guarantees

- Only the worker holding the **current lease ID** may ACK or FAIL a job
- Stale workers are explicitly rejected, even if they previously held the lease
- Lease ownership is versioned, not just time-based
- This prevents zombie workers from committing after lease reassignment

### Failure Testing

- Simulated worker stalls beyond lease expiration
- Verified that:
  - stale workers receive `409 Conflict`
  - newly leased workers with the correct `lease_id` succeed
- Confirmed that lease expiration + re-leasing increments `LeaseID`
- Verified correct behavior under rapid poll/ack races

### Key Takeaway

Time-based leases alone are insufficient in distributed systems.  
By introducing fencing tokens (`LeaseID`), the system guarantees that **only the most recent lease holder can mutate job state**, eliminating zombie worker races and preserving correctness under partial failures.

---

## Day 7 — Observability, Failure Harness, and Long Polling

### Concepts Learned

- Correctness in distributed systems must be **observable**
- Logs are a first-class correctness tool, not just debugging output
- Failure testing is more valuable than unit testing for distributed systems
- Broker restarts are a *designed-for failure*, not an edge case
- Naive polling is inefficient and wasteful at scale
- Long polling is a fundamental optimization used by real queues (e.g., SQS)

---

### What I Built

#### Structured Logging Improvements

- Standardized log events for all state transitions:
  - job_enqueued
  - job_leased
  - job_acked
  - job_failed
  - job_retry_scheduled
  - lease_expired
- Logs consistently include:
  - `job_id`
  - `worker_id` (when applicable)
  - `lease_id`
  - timing metadata (lease expiration, retry delay)
- This allows correctness to be verified purely from logs without stepping through code

---

#### Failure Harness

Built a lightweight failure harness using shell scripts and multiple worker processes
to intentionally induce real-world failure scenarios.

The harness simulates:

- High-volume job enqueue (“spam enqueue”)
- Multiple concurrent workers
- Random ACK / FAIL behavior
- Workers that hang mid-job (simulating crashes or pauses)
- Broker restart mid-flight

This allowed validation of system behavior under:

- partial execution
- duplicate delivery
- stale acknowledgements
- retry storms
- lease expiration races

Correctness was verified by inspecting logs and job state transitions
rather than relying on mock-based tests.

---

#### Broker Restart Semantics

- The broker uses an in-memory job store
- Restarting the broker **intentionally resets all state**
- This was explicitly tested by:
  - enqueueing jobs
  - leasing jobs
  - killing the broker
  - restarting and verifying empty state

This clarified an important distinction:

- **Durability is orthogonal to correctness**
- The system is correct *within a process lifetime*
- Persistence (WAL / DB) would be an additive concern, not a redesign

---

#### Long Polling (`/poll`)

Extended the worker polling mechanism to support **long polling**:

- `/poll` may block up to ~30 seconds if no jobs are immediately available
- Returns immediately if a job becomes available during the wait
- Returns `204 No Content` on timeout

This change:

- Reduces unnecessary polling traffic
- Improves efficiency under low-load conditions
- Mirrors real production queue designs (SQS-style polling)

The implementation was intentionally kept simple to avoid overengineering.

---

### Failure Scenarios Tested

- Worker crash mid-processing
- Worker stalls beyond lease duration
- Stale ACK / FAIL from zombie workers
- Concurrent workers contending for the same job
- Retry storms with backoff and jitter
- Poison messages transitioning to DEAD
- Duplicate enqueue requests (idempotency)
- Broker restart with in-flight work
- Long polling timeout vs immediate delivery

---

### Key Takeaway

Correctness in distributed systems is not proven by happy-path tests.
It is proven by:

- explicit state machines
- observable transitions
- and aggressive failure testing

By the end of Day 7, the system behaves predictably under crashes,
timeouts, retries, duplicates, and restarts — which is the real goal
of distributed systems design.