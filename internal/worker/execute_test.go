package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"forge/internal/store"
	"forge/internal/testutil"
)

// newTestStoreTB opens a dedicated per-package test database (forge_test_worker,
// created and migrated on demand by testutil.PrepareTestDB) and truncates the
// job tables. Each package's tests get their own database, so worker tests
// never race the store/agent suites or the live worker stack on `forge`.
func newTestStoreTB(t *testing.T) (*store.PgStore, func()) {
	t.Helper()
	dbURL := testutil.PrepareTestDB(t, "worker")
	s, err := store.NewPgStore(dbURL)
	if err != nil {
		t.Skipf("skipping worker integration test: db connect failed: %v", err)
	}
	if _, err := s.DB().Exec(`TRUNCATE TABLE job_steps CASCADE; TRUNCATE TABLE jobs CASCADE; TRUNCATE TABLE workers CASCADE;`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s, func() {
		_, _ = s.DB().Exec(`TRUNCATE TABLE job_steps CASCADE; TRUNCATE TABLE jobs CASCADE; TRUNCATE TABLE workers CASCADE;`)
		s.Close()
	}
}

func expireLeaseTB(t *testing.T, s *store.PgStore, jobID any) {
	t.Helper()
	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobID,
	); err != nil {
		t.Fatalf("force lease expiry: %v", err)
	}
}

// TestExecuteJob_CheckpointAndResume proves the U4 resumption contract: after a
// worker crashes partway through a multi-segment job, the reclaiming worker
// resumes from the last completed step — every segment is checkpointed exactly
// once, none re-executed, and the job completes.
func TestExecuteJob_CheckpointAndResume(t *testing.T) {
	s, cleanup := newTestStoreTB(t)
	defer cleanup()
	ctx := context.Background()

	job, err := s.CreateJob(ctx, "segments", json.RawMessage(`{"segments":5}`), 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Worker A claims (mints epoch 1) and starts.
	a, err := s.ClaimJob(ctx, "wrk-a", time.Minute)
	if err != nil || a == nil {
		t.Fatalf("claim A: job=%v err=%v", a, err)
	}
	if err := s.StartJob(ctx, job.ID, a.LeaseEpoch); err != nil {
		t.Fatalf("start A: %v", err)
	}

	// A completes only steps 1–2 before crashing. We checkpoint directly
	// (deterministic) rather than racing runSegment's random timing.
	for i := 1; i <= 2; i++ {
		if _, err := s.RecordStep(ctx, job.ID, a.LeaseEpoch, store.JobStep{
			JobID: job.ID, StepNumber: i, StepType: StepTypeSegment,
			Output: json.RawMessage(`{"step":1}`),
		}); err != nil {
			t.Fatalf("A record step %d: %v", i, err)
		}
	}
	if got, err := s.LastCompletedStep(ctx, job.ID); err != nil || got != 2 {
		t.Fatalf("last completed after A's partial: got=%d err=%v, want 2", got, err)
	}

	// A's lease expires (crash); B reclaims the expired 'running' job (epoch 2).
	expireLeaseTB(t, s, job.ID)
	b, err := s.ClaimJob(ctx, "wrk-b", time.Minute)
	if err != nil || b == nil || b.ID != job.ID {
		t.Fatalf("claim B: job=%v err=%v (expected to reclaim A's expired running job)", b, err)
	}
	if b.LeaseEpoch != a.LeaseEpoch+1 {
		t.Fatalf("reclaim epoch: got %d want %d", b.LeaseEpoch, a.LeaseEpoch+1)
	}
	if err := s.StartJob(ctx, job.ID, b.LeaseEpoch); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// B resumes: executeJob reads LastCompletedStep=2 and runs 3–5 only.
	if err := executeJob(ctx, s, b, b.LeaseEpoch, "wrk-b"); err != nil {
		t.Fatalf("B executeJob: %v", err)
	}
	if err := s.CompleteJob(ctx, job.ID, b.LeaseEpoch); err != nil {
		t.Fatalf("B complete: %v", err)
	}

	// Every segment must be checkpointed exactly once (distinct step numbers 1–5).
	steps, err := s.ListSteps(ctx, job.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 5 {
		t.Fatalf("expected 5 checkpointed steps after resume, got %d", len(steps))
	}
	seen := make(map[int]bool, 5)
	for _, st := range steps {
		if st.Status != store.StepCompleted {
			t.Errorf("step %d status %q, want completed", st.StepNumber, st.Status)
		}
		if seen[st.StepNumber] {
			t.Errorf("step %d appears twice — not exactly-once", st.StepNumber)
		}
		seen[st.StepNumber] = true
	}
	for i := 1; i <= 5; i++ {
		if !seen[i] {
			t.Errorf("missing step %d after resume", i)
		}
	}
	final, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status != store.StatusCompleted {
		t.Errorf("final status %q, want completed", final.Status)
	}
}

// TestRecordStep_FencedAfterDepose proves a deposed worker's RecordStep affects
// zero rows (ErrFenced) while the current owner's succeeds — checkpoints cannot
// be written or corrupted by a zombie.
func TestRecordStep_FencedAfterDepose(t *testing.T) {
	s, cleanup := newTestStoreTB(t)
	defer cleanup()
	ctx := context.Background()

	job, err := s.CreateJob(ctx, "segments", json.RawMessage(`{"segments":3}`), 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a, err := s.ClaimJob(ctx, "wrk-a", time.Minute)
	if err != nil || a == nil {
		t.Fatalf("claim A: %v", err)
	}
	epochA := a.LeaseEpoch
	if err := s.StartJob(ctx, job.ID, epochA); err != nil {
		t.Fatalf("start A: %v", err)
	}

	// Depose A: expire its lease and let B reclaim (epoch 1 -> 2).
	expireLeaseTB(t, s, job.ID)
	if b, err := s.ClaimJob(ctx, "wrk-b", time.Minute); err != nil || b == nil || b.ID != job.ID {
		t.Fatalf("claim B should reclaim, got job=%v err=%v", b, err)
	}
	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// A (stale epoch) cannot checkpoint.
	_, err = s.RecordStep(ctx, job.ID, epochA, store.JobStep{
		JobID: job.ID, StepNumber: 1, StepType: StepTypeSegment, Output: json.RawMessage(`{"step":1}`),
	})
	if !errors.Is(err, store.ErrFenced) {
		t.Fatalf("deposed A RecordStep: want ErrFenced, got %v", err)
	}
	// B (fresh epoch) can.
	if _, err := s.RecordStep(ctx, job.ID, got.LeaseEpoch, store.JobStep{
		JobID: job.ID, StepNumber: 1, StepType: StepTypeSegment, Output: json.RawMessage(`{"step":1}`),
	}); err != nil {
		t.Fatalf("B RecordStep with fresh epoch: %v", err)
	}
}

type mockHandler struct {
	called      bool
	gotWorkerID string
}

func (m *mockHandler) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
	m.called = true
	m.gotWorkerID = workerID
	return nil
}

func TestRegisterHandler(t *testing.T) {
	h := &mockHandler{}
	RegisterHandler("test-custom-task", h)

	job := &store.Job{TaskType: "test-custom-task"}
	err := executeJob(context.Background(), nil, job, 1, "test-worker-123")
	if err != nil {
		t.Fatalf("executeJob custom handler: %v", err)
	}
	if !h.called {
		t.Errorf("custom handler was not called")
	}
	if h.gotWorkerID != "test-worker-123" {
		t.Errorf("got workerID %q, want %q", h.gotWorkerID, "test-worker-123")
	}
}
