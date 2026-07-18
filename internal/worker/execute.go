package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"time"

	"forge/internal/store"
)

// StepTypeSegment is the job_steps.step_type recorded for each Week-3 dummy
// segment. Week 4 replaces "segment" with real plan / tool_call / observation
// step types.
const StepTypeSegment = "segment"

const (
	defaultSegments = 5
	// segmentMin/segmentMax bound each segment's simulated work so a job is long
	// enough to kill mid-job but short enough for tests/demos (≈0.4–1.2s each).
	segmentMinMs = 400
	segmentMaxMs = 1200
)

// executeJob runs a job as a sequence of checkpointed, resumable segments
// (U4: WAL/replay semantics — recovery is resumption, not restart). On a fresh
// claim it starts at segment 1; after a reclaim it resumes from
// LastCompletedStep+1, so already-completed segments are never re-run. Each
// completed segment is fenced by the claim's epoch: if the worker was deposed
// mid-job, RecordStep returns ErrFenced and execution aborts without writing a
// stale checkpoint onto another worker's job.
func executeJob(ctx context.Context, s store.JobStore, job *store.Job, epoch int) error {
	segments := decodeSegmentCount(job.Payload)
	start, err := s.LastCompletedStep(ctx, job.ID)
	if err != nil {
		return err
	}
	slog.Info("executing job",
		"job_id", job.ID,
		"segments", segments,
		"resume_from", start,
		"remaining", segments-start,
	)

	for i := start + 1; i <= segments; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, durMs, err := runSegment(ctx, job, i)
		if err != nil {
			return err
		}
		if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: i,
			StepType:   StepTypeSegment,
			Output:     out,
			DurationMs: durMs,
		}); err != nil {
			return err
		}
		slog.Info("segment checkpointed",
			"job_id", job.ID, "step", i, "of", segments)
	}
	return nil
}

// runSegment simulates one unit of work. In Week 4 this becomes a real
// plan/tool-call/observation step. Returns the segment's JSON output and its
// measured duration in ms.
func runSegment(ctx context.Context, job *store.Job, n int) (json.RawMessage, int, error) {
	d := time.Duration(segmentMinMs+rand.Intn(segmentMaxMs-segmentMinMs)) * time.Millisecond
	t0 := time.Now()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	out, _ := json.Marshal(map[string]any{"step": n, "segment_ms": d.Milliseconds()})
	return out, int(time.Since(t0).Milliseconds()), nil
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
