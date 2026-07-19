package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"forge/internal/store"
)

// Run is the main worker polling loop. It claims jobs from the store, runs each
// under a self-renewing lease (U2), and transitions them to completed/failed.
// lease is the per-job lease duration, renewed every lease/3 while the worker
// is alive. Blocks until ctx is cancelled.
func Run(ctx context.Context, s store.JobStore, workerID string, lease time.Duration) error {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	slog.Info("worker started", "worker_id", workerID, "lease", lease)

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker shutting down", "worker_id", workerID)
			return ctx.Err()
		default:
		}

		job, err := s.ClaimJob(ctx, workerID, lease)
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
			"epoch", job.LeaseEpoch,
		)

		// Transition: claimed → running (fenced by the epoch ClaimJob minted).
		if err := s.StartJob(ctx, job.ID, job.LeaseEpoch); err != nil {
			slog.Error("start job failed", "job_id", job.ID, "error", err)
			continue
		}

		// Execute under a self-renewing lease: a per-job goroutine renews the
		// lease every lease/3 (U2), cancelling the job the instant a renewal is
		// fenced (the worker was deposed) so execution aborts immediately.
		if err := executeWithLease(ctx, s, job, lease); err != nil {
			if errors.Is(err, store.ErrFenced) {
				slog.Warn("worker fenced, abandoning job", "job_id", job.ID)
				continue
			}
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return err // worker-level shutdown
			}
			if ferr := s.FailJob(ctx, job.ID, job.LeaseEpoch, err.Error()); ferr != nil {
				slog.Error("fail job", "job_id", job.ID, "error", ferr)
			}
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

// executeWithLease runs executeJob under a self-renewing lease extender. It
// derives a per-job context whose cancellation cause records fencing, spawns
// the extender goroutine, runs the segments, then stops the extender and waits
// for it to exit — structured concurrency, so a renewal goroutine never
// outlives the job. Returns store.ErrFenced if the worker was deposed (the
// extender detected it mid-renew, or a fenced RecordStep did), else executeJob's
// error (context.Canceled on worker shutdown).
func executeWithLease(ctx context.Context, s store.JobStore, job *store.Job, lease time.Duration) error {
	jobCtx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(context.Canceled)

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		extenderLoop(jobCtx, cancelJob, s, job.ID, job.LeaseEpoch, lease)
	}()

	err := executeJob(jobCtx, s, job, job.LeaseEpoch)

	// Stop the extender and wait for it to fully exit before returning.
	cancelJob(context.Canceled)
	<-renewDone

	// If the extender fenced the job (cause == ErrFenced), surface that even
	// though executeJob saw a bare context.Canceled when it was cancelled.
	if cause := context.Cause(jobCtx); errors.Is(cause, store.ErrFenced) {
		return store.ErrFenced
	}
	return err
}

// extenderLoop renews the job's lease every lease/3 while its worker is alive.
// On a fenced renewal (the worker was deposed — epoch bumped by a reclaim) it
// cancels the job with cause ErrFenced so executeJob aborts immediately. On a
// non-fenced renewal error it stops renewing (the lease expires naturally and
// the reclaim path handles the rest). Exits when the job context is cancelled.
func extenderLoop(ctx context.Context, cancel context.CancelCauseFunc, s store.JobStore,
	jobID uuid.UUID, epoch int, lease time.Duration) {
	interval := lease / 3
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RenewLease(ctx, jobID, epoch, lease); err != nil {
				if errors.Is(err, store.ErrFenced) {
					slog.Warn("lease renewal fenced; cancelling job", "job_id", jobID)
					cancel(store.ErrFenced)
					return
				}
				slog.Error("lease renewal failed; letting lease expire",
					"job_id", jobID, "error", err)
				return
			}
		}
	}
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
