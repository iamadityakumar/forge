package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"forge/internal/clock"
	"forge/internal/store"
)

// Handler is a resumable, fenced, checkpointed executor for one task_type.
// Implementations bake their own dependencies (LLM, tools, config) at construction
// time (in cmd/worker/main.go) and do their own checkpointing via the UNCHANGED
// store.RecordStep / LastCompletedStep / ListSteps — the worker package never imports
// llm/tools/agent, so there is no import cycle.
type Handler interface {
	Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error
}

var (
	handlerMu sync.Mutex
	handlers  = map[string]Handler{}
)

// RegisterHandler binds a task_type → Handler. Called once at worker start (Phase 5).
func RegisterHandler(taskType string, h Handler) {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	handlers[taskType] = h
}

// lookupHandler returns the registered handler, else the built-in segmentHandler.
// Backwards-compat: unknown or legacy task_types — including the Week-3 "segments"
// jobs and the live kill-recovery demo — keep running as segments.
func lookupHandler(taskType string) Handler {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	if h, ok := handlers[taskType]; ok {
		return h
	}
	return segmentHandler{}
}

// executeJob is now a one-line dispatcher (the lease/fence shell in loop.go is unchanged).
func executeJob(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
	return lookupHandler(job.TaskType).Run(ctx, s, job, epoch, workerID)
}

// StepTypeSegment is the job_steps.step_type recorded for each Week-3 dummy
// segment. Week 4 replaces "segment" with real plan / tool_call / observation
// step types.
const StepTypeSegment = "segment"

const (
	defaultSegments = 5
)

// segmentMinMs/segmentMaxMs bound each segment's simulated work so a job is long
// enough to kill mid-job but short enough for tests/demos (≈0.4–1.2s each). They
// are package vars (not consts) so the U7 chaos test can shrink segment work to
// ~tiny durations and run thousands of exactly-once assertions quickly under
// -race. Production code never overrides them, so behavior is unchanged.
var (
	segmentMinMs = 400
	segmentMaxMs = 1200
)

type segmentHandler struct{}

func (segmentHandler) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
	return segmentHandler{}.RunWithClock(ctx, s, job, epoch, workerID, clock.SystemClock{})
}

func (segmentHandler) RunWithClock(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string, clk clock.Clock) error {
	segments := decodeSegmentCount(job.Payload)
	start, err := s.LastCompletedStep(ctx, job.ID)
	if err != nil {
		return err
	}
	slog.Info("executing job",
		"job_id", job.ID,
		"task_type", job.TaskType,
		"segments", segments,
		"resume_from", start,
		"remaining", segments-start,
	)

	for i := start + 1; i <= segments; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, durMs, err := runSegmentWithClock(ctx, clk, job, i)
		if err != nil {
			return err
		}
		if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: i,
			StepType:   StepTypeSegment,
			Output:     out,
			DurationMs: durMs,
			WorkerID:   workerID,
		}); err != nil {
			return err
		}
		slog.Info("segment checkpointed",
			"job_id", job.ID, "step", i, "of", segments)
	}
	return nil
}

// runSegmentWithClock simulates one unit of work using the injected clock.
func runSegmentWithClock(ctx context.Context, clk clock.Clock, _ *store.Job, n int) (json.RawMessage, int, error) {
	span := segmentMaxMs - segmentMinMs
	d := time.Duration(segmentMinMs) * time.Millisecond
	if span > 0 {
		d = time.Duration(segmentMinMs+rand.Intn(span)) * time.Millisecond
	}
	t0 := clk.Now()
	clk.Sleep(d)
	out, _ := json.Marshal(map[string]any{"step": n, "segment_ms": d.Milliseconds()})
	return out, int(clk.Now().Sub(t0).Milliseconds()), nil
}

// runSegment simulates one unit of work using the system clock (for production/legacy).
func runSegment(ctx context.Context, _ *store.Job, n int) (json.RawMessage, int, error) {
	return runSegmentWithClock(ctx, clock.SystemClock{}, nil, n)
}

// decodeSegmentCount reads {"segments": N} from the job payload, defaulting to
// defaultSegments when absent/invalid. Lets a job's length be configured per
// submission (e.g. a long job for the kill-recovery demo).
func decodeSegmentCount(payload json.RawMessage) int {
	var p struct {
		Segments int `json:"segments"`
	}
	if err := json.Unmarshal(payload, &p); err == nil && p.Segments > 0 {
		return p.Segments
	}
	return defaultSegments
}
