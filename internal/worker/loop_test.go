package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"forge/internal/store"
)

// depose triggers reclaim of the given job as if its holder crashed:
// force the lease into the past so the next ClaimJob reclaims it (epoch++),
// and claim from a second worker. Returns the reclaiming claim (new epoch).
func depose(t *testing.T, s *store.PgStore, workerID string, jobID any) *store.Job {
	t.Helper()
	expireLeaseTB(t, s, jobID)
	b, err := s.ClaimJob(context.Background(), workerID, time.Minute)
	if err != nil || b == nil {
		t.Fatalf("depose: reclaim failed job=%v err=%v", b, err)
	}
	return b
}

// TestExecuteWithLease_DeposedWorkerAborts proves the U2 safety side: once a
// worker is deposed (its epoch bumped by a reclaim while it is still running),
// its lease-extender's next RenewLease returns ErrFenced and cancels the job
// context, so executeWithLease aborts with ErrFenced and — critically — does NOT
// complete the job. The zombie stops and stops pinning the job (the new owner
// is free to proceed).
func TestExecuteWithLease_DeposedWorkerAborts(t *testing.T) {
	s, cleanup := newTestStoreTB(t)
	defer cleanup()
	ctx := context.Background()

	// 50 long segments so execution cannot possibly finish before we depose it.
	job, err := s.CreateJob(ctx, "segments", json.RawMessage(`{"segments":50}`), 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a, err := s.ClaimJob(ctx, "wrk-a", time.Minute)
	if err != nil || a == nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := s.StartJob(ctx, job.ID, a.LeaseEpoch); err != nil {
		t.Fatalf("start A: %v", err)
	}

	// Short lease (150ms, renewed every 50ms) so fencing is detected quickly.
	lease := 150 * time.Millisecond
	errCh := make(chan error, 1)
	go func() { errCh <- executeWithLease(ctx, s, a, lease) }()

	// Let the worker run a couple of renewal cycles first (so the extender is
	// genuinely alive and renewing), then depose it with a second claim.
	time.Sleep(120 * time.Millisecond)
	b := depose(t, s, "wrk-b", job.ID)
	if b.LeaseEpoch != a.LeaseEpoch+1 {
		t.Fatalf("reclaim epoch: got %d want %d", b.LeaseEpoch, a.LeaseEpoch+1)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, store.ErrFenced) {
			t.Fatalf("deposed executeWithLease: want ErrFenced, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executeWithLease did not abort within 5s of depose")
	}

	// The job must NOT be completed by the deposed worker.
	final, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status == store.StatusCompleted {
		t.Fatal("deposed worker completed the job it no longer owns")
	}
	// The new owner holds a strictly higher epoch and the job is reclaimed
	// (claimed by wrk-b), not lost.
	if final.LeaseEpoch <= a.LeaseEpoch {
		t.Errorf("epoch not advanced by reclaim: got %d, want > %d", final.LeaseEpoch, a.LeaseEpoch)
	}
	if final.ClaimedBy == nil || *final.ClaimedBy != "wrk-b" {
		t.Errorf("expected claimed_by=wrk-b after reclaim, got %v", final.ClaimedBy)
	}
}

// TestExecuteWithLease_AliveWorkerCompletes proves the U2 liveness side: a job
// whose total runtime clearly exceeds the lease window still completes under
// executeWithLease, because the extender keeps renewing the lease while the
// worker is alive. If renewal were broken (e.g. always fencing), the extender
// would cancel the job on its first tick and this would time out / fence. Runs
// against the real clock but with generous slack.
func TestExecuteWithLease_AliveWorkerCompletes(t *testing.T) {
	s, cleanup := newTestStoreTB(t)
	defer cleanup()
	ctx := context.Background()

	job, err := s.CreateJob(ctx, "segments", json.RawMessage(`{"segments":4}`), 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a, err := s.ClaimJob(ctx, "wrk-a", time.Minute)
	if err != nil || a == nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := s.StartJob(ctx, job.ID, a.LeaseEpoch); err != nil {
		t.Fatalf("start A: %v", err)
	}

	// 1s lease, renewed every ~333ms. Four segments at 0.4–1.2s each
	// (≥1.6s total) comfortably exceed one 1s lease window — so without active
	// renewal a competing reclaimer would eventually steal the job. We don't
	// run a competitor here; the point is that executeWithLease must not fence
	// itself and must drain all 4 segments to completion.
	lease := 1 * time.Second
	if err := executeWithLease(ctx, s, a, lease); err != nil {
		t.Fatalf("alive executeWithLease: want nil, got %v", err)
	}
	if err := s.CompleteJob(ctx, job.ID, a.LeaseEpoch); err != nil {
		t.Fatalf("complete: %v (job may have been falsely reclaimed — renewal broken?)", err)
	}

	final, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status != store.StatusCompleted {
		t.Errorf("final status %q, want completed", final.Status)
	}
	steps, err := s.ListSteps(ctx, job.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 4 {
		t.Errorf("expected 4 checkpointed steps, got %d", len(steps))
	}
}
