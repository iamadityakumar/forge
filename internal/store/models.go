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

	// Week 3 fields — fencing + scheduling + dead-letter.
	LeaseEpoch  int        `json:"lease_epoch"              db:"lease_epoch"`
	RunAt       *time.Time `json:"run_at,omitempty"         db:"run_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"   db:"completed_at"`
	DeadLetter  bool       `json:"dead_letter"              db:"dead_letter"`
}

// JobStep is a single checkpointed, resumable step of a Job (the job_steps
// table). Resumption on reclaim starts at MAX(step_number) WHERE status =
// 'completed' + 1. For Week 3 the step_type is "segment" (dummy work); Week 4
// replaces it with real plan / tool_call / observation steps.
type JobStep struct {
	ID         uuid.UUID       `json:"id"            db:"id"`
	JobID      uuid.UUID       `json:"job_id"         db:"job_id"`
	StepNumber int             `json:"step_number"    db:"step_number"`
	StepType   string          `json:"step_type"      db:"step_type"`
	Input      json.RawMessage `json:"input,omitempty"  db:"input"`
	Output     json.RawMessage `json:"output,omitempty" db:"output"`
	Status     string          `json:"status"         db:"status"`
	DurationMs int             `json:"duration_ms"    db:"duration_ms"`
	CreatedAt  time.Time       `json:"created_at"     db:"created_at"`
	WorkerID   string          `json:"worker_id,omitempty" db:"worker_id"`
}

// JobStep status constants.
const (
	StepRunning   = "running"
	StepCompleted = "completed"
	StepFailed    = "failed"
)

// Job status constants — the only valid values for Job.Status.
const (
	StatusPending   = "pending"
	StatusClaimed   = "claimed"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// StatusDeadLetter is a VIRTUAL filter value (not a real Job.Status) accepted
// by ListJobs: jobs whose execution exhausted max_attempts and were marked
// dead_letter=true (status='failed', dead_letter=true). Surfaced via
// GET /jobs?status=dead_letter. Real jobs are never stored with this value.
const StatusDeadLetter = "dead_letter"

// Worker is the record for each registered worker process.
type Worker struct {
	ID            string    `json:"id"             db:"id"`
	Hostname      string    `json:"hostname"       db:"hostname"`
	LastHeartbeat time.Time `json:"last_heartbeat" db:"last_heartbeat"`
	Status        string    `json:"status"         db:"status"`
}