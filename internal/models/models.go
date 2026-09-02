package models

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
