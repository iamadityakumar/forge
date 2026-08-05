package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("job not found")
	ErrInvalidTransition = errors.New("invalid status transition")
	ErrFenced = errors.New("worker fenced: lease epoch mismatch")
)

type JobStore interface {
	CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (Job, error)
	GetJob(ctx context.Context, id uuid.UUID) (Job, error)
	ListJobs(ctx context.Context, status string, limit int) ([]Job, error)
	CountPendingJobs(ctx context.Context) (int, error)
	ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*Job, error)
	StartJob(ctx context.Context, jobID uuid.UUID, epoch int) error
	CompleteJob(ctx context.Context, jobID uuid.UUID, epoch int) error
	FailJob(ctx context.Context, jobID uuid.UUID, epoch int, reason string) error
	RecordStep(ctx context.Context, jobID uuid.UUID, epoch int, step JobStep) (uuid.UUID, error)
	LastCompletedStep(ctx context.Context, jobID uuid.UUID) (int, error)
	ListSteps(ctx context.Context, jobID uuid.UUID) ([]JobStep, error)
	RecordLLMCall(ctx context.Context, call LLMCall) (uuid.UUID, error)
	ListLLMCalls(ctx context.Context, jobID uuid.UUID) ([]LLMCall, error)
	RenewLease(ctx context.Context, jobID uuid.UUID, epoch int, lease time.Duration) error
	Heartbeat(ctx context.Context, workerID string, hostname string) error
	CountActiveWorkers(ctx context.Context, within time.Duration) (int, error)
	SetTraceContext(ctx context.Context, jobID uuid.UUID, epoch int, tc json.RawMessage) error
	Ping(ctx context.Context) error
	Close() error
}
