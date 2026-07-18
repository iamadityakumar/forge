package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"forge/internal/store"
)

// Run is the main worker polling loop. It claims jobs from the store,
// executes them (dummy sleep for now), and transitions them to completed/failed.
// It blocks until ctx is cancelled.
func Run(ctx context.Context, s store.JobStore, workerID string) error {
	slog.Info("worker started", "worker_id", workerID)

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker shutting down", "worker_id", workerID)
			return ctx.Err()
		default:
		}

		job, err := s.ClaimJob(ctx, workerID, 2*time.Minute)
		if err != nil {
			slog.Error("claim failed", "worker_id", workerID, "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if job == nil {
			// No work available — back off before polling again.
			sleepCtx(ctx, 1*time.Second)
			continue
		}

		slog.Info("claimed job",
			"worker_id", workerID,
			"job_id", job.ID,
			"task_type", job.TaskType,
			"attempt", job.AttemptCount,
		)

		// Transition: claimed → running (fenced by the epoch ClaimJob just minted).
		if err := s.StartJob(ctx, job.ID, job.LeaseEpoch); err != nil {
			slog.Error("start job failed", "job_id", job.ID, "error", err)
			continue
		}

		// Execute the job as a sequence of checkpointed, resumable segments
		// (Week 2: opaque sleep; Week 3+: multi-step, fenced checkpoint loop).
		if err := executeJob(ctx, s, job, job.LeaseEpoch); err != nil {
			if errors.Is(err, store.ErrFenced) {
				slog.Warn("worker fenced during execution, abandoning job", "job_id", job.ID)
				continue
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			_ = s.FailJob(ctx, job.ID, job.LeaseEpoch, err.Error())
			slog.Error("job execution failed", "job_id", job.ID, "error", err)
			continue
		}

		if err := s.CompleteJob(ctx, job.ID, job.LeaseEpoch); err != nil {
			slog.Error("complete job failed", "job_id", job.ID, "error", err)
			continue
		}
		slog.Info("job completed", "worker_id", workerID, "job_id", job.ID)
	}
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
