CREATE TYPE job_state AS ENUM ('QUEUED', 'LEASED', 'DONE', 'DEAD');

CREATE TABLE Jobs (
    id UUID PRIMARY KEY,
    payload TEXT NOT NULL,
    state job_state NOT NULL DEFAULT 'QUEUED',
    
    lease_owner VARCHAR(255),
    lease_expires_at BIGINT,
    lease_id BIGINT NOT NULL DEFAULT 0,

    attempts INT NOT NULL DEFAULT 0,
    max_tries INT NOT NULL DEFAULT 3,
    next_available_at BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_jobs_state_next_available ON Jobs (state, next_available_at);

CREATE TABLE Idems (
    idem_key TEXT PRIMARY KEY,
    job_id UUID,

    status VARCHAR(50) NOT NULL,

    CONSTRAINT fk_job
        FOREIGN KEY (job_id)
        REFERENCES jobs (id)
        ON DELETE SET NULL
);