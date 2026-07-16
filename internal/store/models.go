package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Job is the record created for each submitted task.
// Fields map 1:1 to the jobs Postgres table.
type Job struct {
	ID             uuid.UUID       `json:"id"               db:"id"`
	TaskType       string          `json:"task_type"        db:"task_type"`
	Payload        json.RawMessage `json:"payload"          db:"payload"`
	Status         string          `json:"status"           db:"status"`
	Priority       int             `json:"priority"         db:"priority"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty" db:"idempotency_key"`
	ClaimedBy      *string         `json:"claimed_by,omitempty"      db:"claimed_by"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty" db:"lease_expires_at"`
	AttemptCount   int             `json:"attempt_count"    db:"attempt_count"`
	MaxAttempts    int             `json:"max_attempts"     db:"max_attempts"`
	ErrorMessage   *string         `json:"error_message,omitempty"   db:"error_message"`
	CreatedAt      time.Time       `json:"created_at"       db:"created_at"`
}

// Job status constants — the only valid values for Job.Status.
const (
	StatusPending   = "pending"
	StatusClaimed   = "claimed"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Worker is the record for each registered worker process.
type Worker struct {
	ID            string    `json:"id"             db:"id"`
	Hostname      string    `json:"hostname"       db:"hostname"`
	LastHeartbeat time.Time `json:"last_heartbeat" db:"last_heartbeat"`
	Status        string    `json:"status"         db:"status"`
}
