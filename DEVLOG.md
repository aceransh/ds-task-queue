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

## Day 8 — Relational Schemas & Docker Infrastructure

### Concepts Learned

- Strong typing in relational databases vs. flexible in-memory maps (e.g., handling the `"PENDING"` state dilemma in SQL).
- Time representations in databases (avoiding Y2K38 by mapping Go's `int64` to Postgres `BIGINT`).
- Docker fundamentals: Containerization ensures reproducible infrastructure across different environments ("it works on my machine" is no longer an excuse).
- Docker Compose: Using YAML to define and orchestrate local infrastructure.
- Go's `database/sql` package acts as a generic interface and requires a specific driver (like `[github.com/lib/pq](https://github.com/lib/pq)`) to communicate with Postgres.
- `sql.Open` only validates the connection string format; a physical `Ping()` is required to verify actual database reachability.

### What I Built

- Translated the in-memory `Job` struct and `idem` map into strict PostgreSQL `CREATE TABLE` definitions.
- Utilized Postgres `ENUM` types for job states and `UUID` for primary keys.
- Set up a `./sql/schema.sql` file and mapped it to Docker's `/docker-entrypoint-initdb.d/` volume for automatic database initialization.
- Wrote a `docker-compose.yml` file to provision a local PostgreSQL 15 server.
- Implemented `initDB()` in `main.go` to establish and verify the connection between the Go broker and the Dockerized database.

### The Struggle & Troubleshooting

- Navigated strict YAML syntax rules (e.g., ensuring port mappings like `"5432:5432"` are quoted to prevent parser errors).
- Encountered the classic "Ghost Database" port conflict. The Go application threw a `role "postgres" does not exist` error despite Docker running perfectly.
- Investigated container logs using `docker compose logs` and learned to cleanly wipe volume state using `docker compose down -v`.
- Discovered that a native MacOS background process was hogging port 5432 and intercepting the Go connection before it could reach the Docker container. Resolved by killing the local background service to clear the port.

### Key Takeaway

Infrastructure as code is just as unforgiving as application logic. Moving from in-memory state to a durable database introduces entirely new failure domains—from strict type enforcement to networking and port binding conflicts. A bulletproof application still fails if the infrastructure beneath it is misconfigured.

## Day 9 — ACID Transactions & Idempotency in SQL

### Concepts Learned

- **Database Transactions (`db.Begin`):** Learned how to group multiple SQL operations (inserts and updates) into a single atomic block. If one fails, `defer tx.Rollback()` ensures the database is not left in a corrupted, partial state.
- **SQL Injection Prevention:** Using parameterized queries (`$1`, `$2`) to safely insert variables instead of string concatenation.
- **Atomic Locks via `ON CONFLICT`:** Replaced Go Mutexes with database-level constraints. Using `INSERT ... ON CONFLICT DO NOTHING` allows the database to instantly and safely reject duplicate idempotency keys from concurrent requests.
- **Handling SQL NULLs in Go:** Standard variables panic when reading `NULL` from a database. Learned to use `sql.NullString` to safely scan empty columns (like a missing `job_id` during a `PENDING` state).
- **Silent Failures:** The importance of aggressively checking `err != nil` after every `tx.Exec()`. Failing to check errors inside a transaction leads to partial executions silently succeeding.
- **Transaction Visibility (Race Conditions):** A `jobCond.Signal()` must only be fired *after* `tx.Commit()`. Signaling earlier wakes up workers before the data is actually visible in the database, resulting in missed jobs.

### What I Built

- Completely rewrote the `/enqueue` endpoint to remove the global `jobsMu` and `idemMu` maps.
- Implemented a transactional insert to save the `Job` and update the `idempotency_keys` table atomically.
- Built a fallback read mechanism (`db.QueryRow`) to return `409 Conflict` for pending jobs or `200 OK` with the existing `job_id` for safely retried jobs.

## Day 10 — The Thundering Herd & Advanced Concurrency

### Concepts Learned

- **The "Thundering Herd" Problem:** Learned how concurrent workers querying a database simultaneously can all grab the same job if not properly managed.
- **Row-Level Locking (`FOR UPDATE SKIP LOCKED`):** Used PostgreSQL's native locking mechanism to ensure that a `SELECT` query safely locks a row, allowing concurrent workers to gracefully skip over jobs that are currently being leased by others.
- **Atomic Read-Modify-Write:** Leveraged the `RETURNING` clause in Postgres to execute an `UPDATE` and fetch the modified row's data in a single, atomic network call.
- **The "Lost Wakeup" Problem:** Deep-dived into Go's `sync.Cond` mechanics. Discovered exactly why `Wait()` strictly requires holding a Mutex (to safely add the worker to the sleep queue without missing signals) and why wrapping `Signal()` in that same lock prevents microsecond race conditions between querying the database and falling asleep.
- **Time Paradigms:** Differentiated between HTTP connection lifecycle management (Go's `time.Time` for deadlines/timeouts) and raw database timestamps (Unix Epoch integers for fast `BIGINT` comparisons).
- **Error Handling with `switch`:** Refactored long `if / else if` error chains into clean `switch err` blocks to safely handle `sql.ErrNoRows` versus fatal database errors without redundant logic.

### What I Built

- Completely rewrote the `/poll` endpoint to remove the global `jobsMu` lock from the database query, allowing Postgres to handle concurrent reads natively.
- Implemented a complex SQL transaction to safely find, lease, and return a queued job in one step.
- Built a hybrid polling mechanism: workers check the database, and if empty, sleep using `jobCond.Wait()` while respecting a strict HTTP deadline via `time.AfterFunc` to prevent infinite hanging.

## Day 10 — Row-Level Locking & Transactional State Machines

### Concepts Learned

* **Row-Level Locking (`FOR UPDATE`):** Discovered the massive race condition in "Read-Check-Update" flows. Solved it using Postgres's `SELECT ... FOR UPDATE` within a transaction, which physically locks the specific row from other workers until the Go logic finishes validating leases and commits the update.
* **Handling Database NULLs (`COALESCE`):** Learned that Go's standard variables (like strings and integers) will panic if they scan a `NULL` from the database. Utilized the SQL `COALESCE()` function to safely default `NULL` lease owners to empty strings.
* **Error Handling (`sql.ErrNoRows`):** Implemented strict checks for `sql.ErrNoRows` during `QueryRow` executions to properly return `404 Not Found` HTTP status codes for invalid worker requests.
* **DRY Query Execution:** Optimized Go code by calculating state variables (like next available time and job state) within Go's control flow, allowing the transaction to execute a single, unified `UPDATE` query instead of repeating SQL strings.

### What I Built

* Successfully migrated the `/ack` endpoint to a transactional SQL model, ensuring that jobs are only marked as `DONE` if the worker still holds a valid, unexpired lease.
* Successfully migrated the `/fail` endpoint, rebuilding the Dead Letter Queue (DLQ) routing and exponential backoff retry scheduling entirely within safe database transactions.

## Day 11 — Stateless Endpoints & Background Sweepers

### Concepts Learned

* **Querying Multiple Rows (`db.Query`):** Transitioned from `QueryRow` (single result) to iterating over `rows.Next()` to return multiple records. Learned the critical importance of `defer rows.Close()` to prevent database connection leaks.
* **Bulk SQL Updates (`RETURNING`):** Replaced an inefficient Go `for` loop that updated records one by one with a single, atomic SQL `UPDATE` statement. Used the `RETURNING id` clause to get immediate feedback from Postgres on which rows were modified.
* **State Synchronization:** Reinforced that `jobCond.Signal()` must always happen *after* `tx.Commit()`. Furthermore, when expiring multiple jobs simultaneously, looping through the expired IDs to call `jobCond.Signal()` ensures the exact correct number of sleeping workers are woken up.
* **Complete Statelessness:** Achieved a fully stateless application layer. The Go server no longer holds any state in RAM, meaning it can be horizontally scaled, killed, or restarted without losing a single job.

### What I Built

* Rewrote the `/jobs` and `/dead` administrative endpoints using `db.Query` to pull directly from the database, effectively replacing the old global mutex maps.
* Safely deleted all global in-memory maps (`jobs`, `idem`) and their corresponding Mutexes.
* Refactored the `expireLeases` background goroutine to execute a bulk, lock-safe database sweep (`FOR UPDATE SKIP LOCKED`) that instantly re-queues jobs from crashed or timed-out workers.

## Day 12 — Containerization, Graceful Shutdowns & Observability

### Concepts Learned

* **Container Orchestration (`docker-compose`):** Learned how to network multiple services (Go backend, PostgreSQL, Prometheus, Grafana) together using Docker Compose. Discovered that the Go application often boots faster than the database, requiring an explicit connection retry loop on startup to prevent crash loops.
* **Asynchronous Servers & OS Signals:** Realized that `http.ListenAndServe` blocks the main execution thread. Moved the HTTP server into a background goroutine and used Go's `os/signal` package and channels to trap `SIGINT` and `SIGTERM` signals. This prevents the operating system from forcefully killing the app and dropping active database transactions.
* **Graceful Shutdown (`context`):** Utilized `context.WithTimeout` paired with the server's built-in `.Shutdown()` method. This safely rejects new incoming HTTP requests while giving active handlers a strict deadline (e.g., 5 seconds) to finish their in-flight database operations before exiting.
* **The Pull Model (Prometheus):** Shifted from a push-based telemetry mindset to a pull-based one. Learned how Prometheus periodically scrapes a `/metrics` endpoint, allowing the Go application to efficiently maintain lightweight in-memory counters (`prometheus.NewCounter`) without incurring the network overhead of pushing data.

### What I Built

* Wrote a `Dockerfile` for the Go application and a `docker-compose.yml` file to spin up the entire production-like infrastructure (Broker, Postgres, Prometheus, Grafana) with a single command.
* Re-architected `main.go` to support graceful shutdowns, ensuring active requests finish and the PostgreSQL connection pool (`db.Close()`) is cleanly severed before the process exits.
* Instrumented the application with the Prometheus Go client, creating counters to track total enqueued, acknowledged, and failed jobs.
* Exposed the telemetry via `promhttp.Handler()` and strategically placed the `.Inc()` metric calls strictly *after* successful database `tx.Commit()` calls to guarantee 100% accurate dashboard telemetry.

## Day 13 — Package Structure, a Real Worker Binary, and Integration Tests

### Concepts Learned

- A single `main.go` mixing HTTP handlers, DB access, and shared types is fine for a prototype, but it blocks testing: package-level globals (`db`, `jobsMu`, `jobCond`) mean every test shares the same mutable state.
- Dependency injection via a struct (rather than package globals) lets each test spin up its own isolated instance pointed at its own connection.
- Integration tests for a system built on `FOR UPDATE SKIP LOCKED` and row-level locking are only meaningful against a real database — mocking `database/sql` would test the mock, not the locking behavior the system actually depends on.
- A worker that's "just curl in a bash loop" is fine for failure injection, but a real client binary exercises the same HTTP/JSON contract the broker promises, including context cancellation and graceful shutdown on the client side.

### What I Built

- Split the single `main.go` into `cmd/broker` (the HTTP API binary), `cmd/worker` (a new Go binary that polls, "executes," and acks/fails jobs — previously this only existed as `scripts/worker_loop.sh`), `internal/db` (connection setup), and `internal/models` (shared request/response structs and the `JobState` enum).
- Converted the broker's handlers from package-level `http.HandleFunc` closures over global state into methods on a `Server` struct (`db *sql.DB`, `jobsMu sync.Mutex`, `jobCond *sync.Cond`), constructed via `NewServer(db)`. The long-polling wakeup mechanism (`jobCond`) still holds no job state itself — only Postgres does.
- Added `internal/testutil.NewTestDB(t)`, which opens a connection to the real Postgres instance, truncates `Jobs` and `Idems`, and registers cleanup — no mocks.
- Wrote 13 integration tests in `cmd/broker/broker_test.go` covering enqueue→QUEUED, poll→LEASED, ack→DONE, health, `/jobs`, `/dead`, stale `lease_id` rejection on both ack and fail, expired-lease rejection on both ack and fail, fail→QUEUED with backoff, DLQ transition at `max_tries`, and `Sweep()` re-queuing expired leases directly (no need to sleep out a real lease timeout in a test).
- Fixed a retry-loop bug in `InitDB()`: the original fatal check ran *after* a successful ping, so it could never fire, and a fully exhausted retry loop left `db` as `nil` instead of crashing loudly. The fatal check now fires on the last failed ping attempt instead.
- Added `idx_jobs_state_next_available` on `(state, next_available_at)` — the exact pair `/poll`'s `FOR UPDATE SKIP LOCKED` query filters on.

### Key Takeaway

Testing a concurrency-and-locking-heavy system honestly means testing against the real database, not a mock of the interface. Getting there required removing the global state that made the handlers untestable in isolation — the `Server` struct isn't just a style preference, it's what makes `NewServer(freshTestDB)` per-test possible.