package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors returned by JobStore implementations.
var (
	// ErrNotFound is returned when a requested job does not exist.
	ErrNotFound = errors.New("job not found")

	// ErrInvalidTransition is returned when a state transition is not allowed
	// (e.g. trying to start an already-completed job).
	ErrInvalidTransition = errors.New("invalid status transition")

	// ErrFenced is returned when a fenced write affects zero rows because the
	// caller's lease_epoch no longer matches the row's — i.e. the worker was
	// deposed (fenced) by a reclaim that bumped lease_epoch. The caller must
	// abandon the job immediately rather than mutate it further. This is how
	// double-execution is prevented by construction (Kleppmann fencing tokens).
	ErrFenced = errors.New("worker fenced: lease epoch mismatch")
)

// JobStore is the persistence interface used by both the API server and workers.
// All methods are safe for concurrent use.
//
// Every mutation that presumes ownership of a job (StartJob, CompleteJob,
// FailJob) is fenced: the caller must pass the lease_epoch it received from
// ClaimJob as a fencing token, and the write only lands if the row's epoch still
// matches. A mismatch yields ErrFenced.
type JobStore interface {
	// CreateJob inserts a new job. If idempotencyKey is non-empty and already
	// exists, the existing job is returned instead of creating a duplicate.
	CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (Job, error)

	// GetJob returns a single job by ID.
	GetJob(ctx context.Context, id uuid.UUID) (Job, error)

	// ListJobs returns jobs filtered by status (empty string = all).
	ListJobs(ctx context.Context, status string, limit int) ([]Job, error)

	// ClaimJob atomically claims the next available job for the given worker
	// using SKIP LOCKED, mints a new fencing token (lease_epoch + 1) which the
	// caller must use on subsequent fenced writes, and returns nil, nil when no
	// claimable job exists.
	ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*Job, error)

	// StartJob transitions a job from claimed → running, fenced by epoch.
	StartJob(ctx context.Context, jobID uuid.UUID, epoch int) error

	// CompleteJob transitions a job from running → completed, fenced by epoch.
	CompleteJob(ctx context.Context, jobID uuid.UUID, epoch int) error

	// FailJob transitions a job from running → failed and records the reason,
	// fenced by epoch.
	FailJob(ctx context.Context, jobID uuid.UUID, epoch int, reason string) error

	// RecordStep checkpoint-writes a single step of a job, fenced by epoch. The
	// write only lands if the job's lease_epoch still matches; a mismatch (the
	// worker was deposed) returns ErrFenced. Idempotent: re-recording the same
	// step_number updates the row in place via ON CONFLICT.
	RecordStep(ctx context.Context, jobID uuid.UUID, epoch int, step JobStep) (uuid.UUID, error)

	// LastCompletedStep returns MAX(step_number) WHERE status='completed', or 0
	// if none — the resumption point a reclaimed job starts from (+1).
	LastCompletedStep(ctx context.Context, jobID uuid.UUID) (int, error)

	// ListSteps returns the ordered steps of a job (for GET /jobs/{id}/trace).
	ListSteps(ctx context.Context, jobID uuid.UUID) ([]JobStep, error)

	// Heartbeat upserts a worker's heartbeat timestamp.
	Heartbeat(ctx context.Context, workerID string, hostname string) error

	// Ping checks database connectivity.
	Ping(ctx context.Context) error

	// Close releases the underlying database connection.
	Close() error
}
