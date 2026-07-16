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
)

// JobStore is the persistence interface used by both the API server and workers.
// All methods are safe for concurrent use.
type JobStore interface {
	// CreateJob inserts a new job. If idempotencyKey is non-empty and already
	// exists, the existing job is returned instead of creating a duplicate.
	CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (Job, error)

	// GetJob returns a single job by ID.
	GetJob(ctx context.Context, id uuid.UUID) (Job, error)

	// ListJobs returns jobs filtered by status (empty string = all).
	ListJobs(ctx context.Context, status string, limit int) ([]Job, error)

	// ClaimJob atomically claims the next available job for the given worker
	// using SKIP LOCKED. Returns nil, nil when no claimable job exists.
	ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*Job, error)

	// StartJob transitions a job from claimed → running.
	StartJob(ctx context.Context, jobID uuid.UUID) error

	// CompleteJob transitions a job from running → completed.
	CompleteJob(ctx context.Context, jobID uuid.UUID) error

	// FailJob transitions a job from running → failed and records the reason.
	FailJob(ctx context.Context, jobID uuid.UUID, reason string) error

	// Heartbeat upserts a worker's heartbeat timestamp.
	Heartbeat(ctx context.Context, workerID string, hostname string) error

	// Ping checks database connectivity.
	Ping(ctx context.Context) error

	// Close releases the underlying database connection.
	Close() error
}
