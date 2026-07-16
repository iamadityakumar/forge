package worker

import (
	"context"
	"log/slog"
	"math/rand"
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

		// Transition: claimed → running
		if err := s.StartJob(ctx, job.ID); err != nil {
			slog.Error("start job failed", "job_id", job.ID, "error", err)
			continue
		}

		// Execute the job (dummy for Week 2, replaced with agent loop in Week 4).
		if err := executeJob(ctx, job); err != nil {
			_ = s.FailJob(ctx, job.ID, err.Error())
			slog.Error("job execution failed", "job_id", job.ID, "error", err)
			continue
		}

		if err := s.CompleteJob(ctx, job.ID); err != nil {
			slog.Error("complete job failed", "job_id", job.ID, "error", err)
			continue
		}
		slog.Info("job completed", "worker_id", workerID, "job_id", job.ID)
	}
}

// executeJob simulates work. In Week 4 this is replaced with the real
// plan → tool call → observe agent loop.
func executeJob(ctx context.Context, job *store.Job) error {
	d := time.Duration(1+rand.Intn(3)) * time.Second
	slog.Info("executing job (simulated)", "job_id", job.ID, "duration", d)
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
