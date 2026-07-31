package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

	"forge/internal/store"
)

// defaultWorkerConcurrency keeps a worker serial when WORKER_CONCURRENCY is
// unset — the safe Week-2 baseline. Set concurrency > 1 (U6) to run that many
// jobs at once per worker behind a bounded semaphore.
const defaultWorkerConcurrency = 1

// Run is the main worker polling loop. It claims jobs from the store and runs
// each under a self-renewing lease (U2), transitioning them to completed/failed.
//
// Up to concurrency jobs run at once (U6 bounded per-worker concurrency): the
// loop claims a new job as soon as a concurrency slot is free, then runs it in
// its own goroutine that owns its lease-renewal goroutine + fenced step loop.
// All in-flight jobs are rooted in this worker's ctx, so a SIGINT/SIGTERM (or a
// kill -9 that cancels the root) cancels them all at once. lease is the per-job
// lease duration, renewed every lease/3 while each job runs. Blocks until ctx is
// cancelled, after waiting for every in-flight job + lease goroutine to unwind
// (structured concurrency: no goroutine outlives Run).
func Run(ctx context.Context, s store.JobStore, workerID string, lease time.Duration, concurrency int) error {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	if concurrency < 1 {
		concurrency = defaultWorkerConcurrency
	}

	var wg sync.WaitGroup
	sem := semaphore.NewWeighted(int64(concurrency))

	slog.Info("worker started", "worker_id", workerID, "lease", lease, "concurrency", concurrency)

	for {
		// Acquire a concurrency slot, responsive to shutdown. When ctx is
		// cancelled (graceful drain or kill) Acquire returns ctx.Err(); we then
		// wait for in-flight jobs to abort before returning.
		if err := sem.Acquire(ctx, 1); err != nil {
			wg.Wait()
			slog.Info("worker shutting down", "worker_id", workerID)
			return ctx.Err()
		}
		// Re-check after acquiring: a shutdown that raced the acquire should not
		// start a brand-new job.
		if err := ctx.Err(); err != nil {
			sem.Release(1)
			wg.Wait()
			slog.Info("worker shutting down", "worker_id", workerID)
			return err
		}

		job, err := s.ClaimJob(ctx, workerID, lease)
		if err != nil {
			slog.Error("claim failed", "worker_id", workerID, "error", err)
			sem.Release(1)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if job == nil {
			// No work available — back off (with a little jitter so N concurrent
			// pollers / N workers don't all hammer the queue in lockstep).
			sem.Release(1)
			sleepCtx(ctx, idleBackoff())
			continue
		}

		slog.Info("claimed job",
			"worker_id", workerID,
			"job_id", job.ID,
			"task_type", job.TaskType,
			"attempt", job.AttemptCount,
			"epoch", job.LeaseEpoch,
		)

		// Claimed — run it in its own goroutine holding the concurrency slot.
		// WaitGroup ensures Run doesn't return until this job (and its lease
		// goroutine) have fully unwound on shutdown.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sem.Release(1)
			runOneJob(ctx, s, workerID, job, lease)
		}()
	}
}

// runOneJob is the per-job body formerly inlined in the serial Run loop: start
// the job (fenced), execute it under a self-renewing lease, and complete/fail it
// — all rooted in the worker ctx so a shutdown aborts in-flight work. On a fence
// or shutdown it abandons without mutating the job (someone else now owns it, or
// the worker is dying and the leave-for-reclaim path handles it). On any other
// error it records the failure so the retry/DLQ machinery (U5) decides the fate.
func runOneJob(ctx context.Context, s store.JobStore, workerID string, job *store.Job, lease time.Duration) {
	// Transition: claimed → running (fenced by the epoch ClaimJob minted).
	if err := s.StartJob(ctx, job.ID, job.LeaseEpoch); err != nil {
		// ErrFenced here means another worker already reclaimed between our claim
		// and start — leave it to them. Shutdown errors likewise just abandon.
		if errors.Is(err, store.ErrFenced) {
			slog.Warn("start job fenced, abandoning", "job_id", job.ID, "error", err)
			return
		}
		if err := ctx.Err(); err != nil {
			slog.Info("worker shutting down before start", "job_id", job.ID)
			return
		}
		slog.Error("start job failed", "job_id", job.ID, "error", err)
		return
	}

	// Execute under a self-renewing lease: a per-job goroutine renews the
	// lease every lease/3 (U2), cancelling the job the instant a renewal is
	// fenced (the worker was deposed) so execution aborts immediately.
	err := executeWithLease(ctx, s, job, lease, workerID)
	if err == nil {
		if err := s.CompleteJob(ctx, job.ID, job.LeaseEpoch); err != nil {
			if errors.Is(err, store.ErrFenced) {
				slog.Warn("complete job fenced (reclaimed mid-complete), abandoning", "job_id", job.ID)
				return
			}
			slog.Error("complete job failed", "job_id", job.ID, "error", err)
			return
		}
		slog.Info("job completed", "worker_id", workerID, "job_id", job.ID)
		return
	}

	switch {
	case errors.Is(err, store.ErrFenced):
		slog.Warn("worker fenced, abandoning job", "job_id", job.ID)
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// Worker-level shutdown (SIGINT/SIGTERM or kill -9 cancel) — abandon
		// in-flight work; the job is left running/claimed for reclaim, NOT
		// failed, so it resumes cleanly under a new owner.
		slog.Info("worker shutting down, leaving job for reclaim", "job_id", job.ID)
	default:
		// A genuine execution error: record it so FailJob decides requeue vs
		// dead-letter (U5). Only fail if we still own it.
		if ferr := s.FailJob(ctx, job.ID, job.LeaseEpoch, err.Error()); ferr != nil {
			if errors.Is(ferr, store.ErrFenced) {
				slog.Warn("fail job fenced (reclaimed), abandoning", "job_id", job.ID)
				return
			}
			slog.Error("fail job", "job_id", job.ID, "error", ferr)
		}
		slog.Error("job execution failed", "job_id", job.ID, "error", err)
	}
}

// idleBackoff returns a short jittered sleep between polls when no job is
// claimable, so multiple workers/concurrent pollers don't synchronized-thunder.
func idleBackoff() time.Duration {
	return time.Second + time.Duration(rand.Int63n(int64(300*time.Millisecond)))
}

// executeWithLease runs executeJob under a self-renewing lease extender. It
// derives a per-job context whose cancellation cause records fencing, spawns
// the extender goroutine, runs the segments, then stops the extender and waits
// for it to exit — structured concurrency, so a renewal goroutine never
// outlives the job. Returns store.ErrFenced if the worker was deposed (the
// extender detected it mid-renew, or a fenced RecordStep did), else executeJob's
// error (context.Canceled on worker shutdown).
func executeWithLease(ctx context.Context, s store.JobStore, job *store.Job, lease time.Duration, workerID string) error {
	jobCtx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(context.Canceled)

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		extenderLoop(jobCtx, cancelJob, s, job.ID, job.LeaseEpoch, lease)
	}()

	err := executeJob(jobCtx, s, job, job.LeaseEpoch, workerID)

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
