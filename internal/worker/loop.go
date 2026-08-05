package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"

	"forge/internal/metrics"
	"forge/internal/store"
	"forge/internal/trace"
)

const defaultWorkerConcurrency = 1
const heartbeatInterval = 10 * time.Second

func Run(ctx context.Context, s store.JobStore, workerID string, lease time.Duration, concurrency int, m *metrics.Metrics) error {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	if concurrency < 1 {
		concurrency = defaultWorkerConcurrency
	}

	var wg sync.WaitGroup
	sem := semaphore.NewWeighted(int64(concurrency))

	slog.Info("worker started", "worker_id", workerID, "lease", lease, "concurrency", concurrency)

	// Start periodic heartbeat goroutine
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		heartbeatLoop(ctx, s, workerID)
	}()

	defer func() {
		<-hbDone
	}()

	for {
		// ... rest of function
		if err := sem.Acquire(ctx, 1); err != nil {
			wg.Wait()
			slog.Info("worker shutting down", "worker_id", workerID)
			return ctx.Err()
		}
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
			sem.Release(1)
			sleepCtx(ctx, idleBackoff())
			continue
		}

		if m != nil {
			m.ClaimsTotal.Inc()
		}

		slog.Info("claimed job",
			"worker_id", workerID,
			"job_id", job.ID,
			"task_type", job.TaskType,
			"attempt", job.AttemptCount,
			"epoch", job.LeaseEpoch,
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sem.Release(1)
			runOneJob(ctx, s, workerID, job, lease, m)
		}()
	}
}

func runOneJob(ctx context.Context, s store.JobStore, workerID string, job *store.Job, lease time.Duration, m *metrics.Metrics) {
	tracer := trace.NewTracer("worker-" + workerID)

	var jobCtx context.Context
	var jobSpan *trace.Span

	if len(job.TraceContext) > 0 {
		jobCtx, jobSpan = tracer.ExtractContext(ctx, job.TraceContext)
		jobCtx, jobSpan = tracer.StartSpan(jobCtx, "reclaim",
			trace.Attribute{Key: "job_id", Value: job.ID.String()},
			trace.Attribute{Key: "worker_id", Value: workerID},
			trace.Attribute{Key: "attempt", Value: job.AttemptCount},
			trace.Attribute{Key: "epoch", Value: job.LeaseEpoch},
		)
	} else {
		jobCtx, jobSpan = tracer.StartSpan(ctx, "job.run",
			trace.Attribute{Key: "job_id", Value: job.ID.String()},
			trace.Attribute{Key: "worker_id", Value: workerID},
			trace.Attribute{Key: "task_type", Value: job.TaskType},
			trace.Attribute{Key: "attempt", Value: job.AttemptCount},
			trace.Attribute{Key: "epoch", Value: job.LeaseEpoch},
		)
		tcRaw := trace.MarshalContext(jobSpan)
		_ = s.SetTraceContext(jobCtx, job.ID, job.LeaseEpoch, tcRaw)
	}
	defer jobSpan.End()

	if m != nil {
		m.InFlightJobs.Inc()
		defer m.InFlightJobs.Dec()
	}
	jobStart := time.Now()

	if err := s.StartJob(jobCtx, job.ID, job.LeaseEpoch); err != nil {
		if errors.Is(err, store.ErrFenced) {
			jobSpan.SetStatus("error", err)
			slog.Warn("start job fenced, abandoning", "job_id", job.ID, "error", err)
			return
		}
		if err := ctx.Err(); err != nil {
			jobSpan.SetStatus("canceled", err)
			slog.Info("worker shutting down before start", "job_id", job.ID)
			return
		}
		jobSpan.SetStatus("error", err)
		slog.Error("start job failed", "job_id", job.ID, "error", err)
		return
	}

	err := executeWithLease(jobCtx, s, job, lease, workerID, m)
	if err == nil {
		if err := s.CompleteJob(jobCtx, job.ID, job.LeaseEpoch); err != nil {
			if errors.Is(err, store.ErrFenced) {
				jobSpan.SetStatus("error", err)
				slog.Warn("complete job fenced (reclaimed mid-complete), abandoning", "job_id", job.ID)
				return
			}
			jobSpan.SetStatus("error", err)
			slog.Error("complete job failed", "job_id", job.ID, "error", err)
			return
		}
		dur := time.Since(jobStart)
		jobSpan.SetStatus("ok", nil)
		if m != nil {
			m.JobsCompleted.Inc()
			m.JobDuration.Observe(dur.Seconds())
		}
		slog.Info("job completed", "worker_id", workerID, "job_id", job.ID)
		return
	}

	switch {
	case errors.Is(err, store.ErrFenced):
		jobSpan.SetStatus("error", err)
		slog.Warn("worker fenced, abandoning job", "job_id", job.ID)
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		jobSpan.SetStatus("canceled", err)
		slog.Info("worker shutting down, leaving job for reclaim", "job_id", job.ID)
	default:
		jobSpan.SetStatus("error", err)
		deadLetter := job.AttemptCount >= job.MaxAttempts
		if m != nil {
			m.JobsFailed.WithLabelValues(strconv.FormatBool(deadLetter)).Inc()
		}

		if ferr := s.FailJob(jobCtx, job.ID, job.LeaseEpoch, err.Error()); ferr != nil {
			if errors.Is(ferr, store.ErrFenced) {
				slog.Warn("fail job fenced (reclaimed), abandoning", "job_id", job.ID)
				return
			}
			slog.Error("fail job", "job_id", job.ID, "error", ferr)
		}
		slog.Error("job execution failed", "job_id", job.ID, "error", err)
	}
}

func idleBackoff() time.Duration {
	return time.Second + time.Duration(rand.Int63n(int64(300*time.Millisecond)))
}

func executeWithLease(ctx context.Context, s store.JobStore, job *store.Job, lease time.Duration, workerID string, m *metrics.Metrics) error {
	jobCtx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(context.Canceled)

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		extenderLoop(jobCtx, cancelJob, s, job.ID, job.LeaseEpoch, lease, m)
	}()

	err := executeJob(jobCtx, s, job, job.LeaseEpoch, workerID)

	cancelJob(context.Canceled)
	<-renewDone

	if cause := context.Cause(jobCtx); errors.Is(cause, store.ErrFenced) {
		return store.ErrFenced
	}
	return err
}

func extenderLoop(ctx context.Context, cancel context.CancelCauseFunc, s store.JobStore,
	jobID uuid.UUID, epoch int, lease time.Duration, m *metrics.Metrics) {
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
			if m != nil {
				m.LeaseExtensions.Inc()
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func heartbeatLoop(ctx context.Context, s store.JobStore, workerID string) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	hostname, _ := os.Hostname()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Heartbeat(ctx, workerID, hostname); err != nil {
				slog.Warn("heartbeat failed", "worker_id", workerID, "error", err)
			}
		}
	}
}