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

	// Week 6 field — OpenTelemetry trace context propagation.
	TraceContext json.RawMessage `json:"trace_context,omitempty" db:"trace_context"`
}

// JobCounts is an aggregate snapshot of job rows by lifecycle status.
type JobCounts struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	Running    int `json:"running"` // claimed + running (in-flight)
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	DeadLetter int `json:"dead_letter"`
}

// JobStep is a single checkpointed, resumable step of a Job.
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

// LLMCall tracks an individual LLM call performed for a job.
type LLMCall struct {
	ID               uuid.UUID `json:"id"                db:"id"`
	JobID            uuid.UUID `json:"job_id"            db:"job_id"`
	WorkerID         *string   `json:"worker_id,omitempty" db:"worker_id"`
	Backend          string    `json:"backend"           db:"backend"`
	PromptTokens     int       `json:"prompt_tokens"     db:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens" db:"completion_tokens"`
	EstimatedTokens  int       `json:"estimated_tokens"  db:"estimated_tokens"`
	LatencyMs        int       `json:"latency_ms"        db:"latency_ms"`
	Error            *string   `json:"error,omitempty"    db:"error"`
	CreatedAt        time.Time `json:"created_at"        db:"created_at"`
}

const (
	StepRunning   = "running"
	StepCompleted = "completed"
	StepFailed    = "failed"
)

const (
	StatusPending   = "pending"
	StatusClaimed   = "claimed"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

const StatusDeadLetter = "dead_letter"

type Worker struct {
	ID            string    `json:"id"             db:"id"`
	Hostname      string    `json:"hostname"       db:"hostname"`
	LastHeartbeat time.Time `json:"last_heartbeat" db:"last_heartbeat"`
	Status        string    `json:"status"         db:"status"`
}
