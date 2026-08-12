package sim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"forge/internal/clock"
	"forge/internal/store"
)

// TestSim_LeaseExpiryWhileAlive reproduces U2/U3: a paused worker whose lease
// expires while it is still alive gets fenced when it tries to renew.
func TestSim_LeaseExpiryWhileAlive(t *testing.T) {
	clk := clock.NewManualClock(time.Unix(1700000000, 0))
	s := NewSimStore(clk)

	ctx := context.Background()
	// Worker A claims and starts a long job
	j, err := s.CreateJob(ctx, "segments", []byte(`{"segments":20}`), 0, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimedA, err := s.ClaimJob(ctx, "worker-a", 150*time.Millisecond)
	if err != nil || claimedA == nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := s.StartJob(ctx, j.ID, claimedA.LeaseEpoch); err != nil {
		t.Fatalf("start A: %v", err)
	}
	jobID := j.ID

	// Let A run a couple of renewal cycles
	clk.Advance(100 * time.Millisecond)
	// A's extender would renew here (lease/3 = 50ms)
	clk.Advance(50 * time.Millisecond) // 150ms total - at lease boundary
	// A's extender renews - lease moves to 300ms
	if err := s.RenewLease(ctx, jobID, claimedA.LeaseEpoch, 150*time.Millisecond); err != nil {
		t.Fatalf("renew A: %v", err)
	}

	// NOW: SUSPEND worker A's extender - A is alive but not renewing
	// Advance past the 150ms lease (now at 300ms, next expiry at 450ms)
	clk.Advance(200 * time.Millisecond) // 500ms total

	// Worker B should now be able to claim the expired running job
	claimedB, err := s.ClaimJob(ctx, "worker-b", 150*time.Millisecond)
	if err != nil || claimedB == nil {
		t.Fatalf("claim B: %v", err)
	}
	if claimedB.ID != jobID {
		t.Fatalf("expected B to reclaim same job, got %v", claimedB.ID)
	}
	if claimedB.LeaseEpoch <= claimedA.LeaseEpoch {
		t.Errorf("expected reclaim to bump epoch > %d, got %d", claimedA.LeaseEpoch, claimedB.LeaseEpoch)
	}

	// A tries to renew with stale epoch - should be fenced
	err = s.RenewLease(ctx, jobID, claimedA.LeaseEpoch, 150*time.Millisecond)
	if !errors.Is(err, store.ErrFenced) {
		t.Errorf("expected ErrFenced from deposed worker's RenewLease, got %v", err)
	}

	// B starts and completes the job
	if err := s.StartJob(ctx, jobID, claimedB.LeaseEpoch); err != nil {
		t.Fatalf("start B: %v", err)
	}
	if err := s.CompleteJob(ctx, jobID, claimedB.LeaseEpoch); err != nil {
		t.Fatalf("complete B: %v", err)
	}

	j, err = s.GetJob(ctx, jobID)
	if err != nil || j.ID == uuid.Nil || j.Status != store.StatusCompleted {
		t.Errorf("expected job completed, got %v", j)
	}
}

// TestSim_FencingTokenRace reproduces U1: a deposed zombie's writes are fenced.
func TestSim_FencingTokenRace(t *testing.T) {
	clk := clock.NewManualClock(time.Unix(1700000000, 0))
	s := NewSimStore(clk)

	ctx := context.Background()
	j, err := s.CreateJob(ctx, "segments", []byte(`{"segments":10}`), 0, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimedA, err := s.ClaimJob(ctx, "worker-a", 150*time.Millisecond)
	if err != nil || claimedA == nil {
		t.Fatalf("claim A: %v", err)
	}
	if err := s.StartJob(ctx, j.ID, claimedA.LeaseEpoch); err != nil {
		t.Fatalf("start A: %v", err)
	}
	jobID := j.ID
	epochA := claimedA.LeaseEpoch

	// Advance past lease so B can reclaim
	clk.Advance(200 * time.Millisecond)

	claimedB, err := s.ClaimJob(ctx, "worker-b", 150*time.Millisecond)
	if err != nil || claimedB == nil {
		t.Fatalf("claim B: %v", err)
	}
	if claimedB.ID != jobID {
		t.Fatalf("expected B to reclaim same job, got %v", claimedB.ID)
	}
	epochB := claimedB.LeaseEpoch

	// Now A (holding epochA) tries to complete the job — should be fenced
	err = s.CompleteJob(ctx, jobID, epochA)
	if !errors.Is(err, store.ErrFenced) {
		t.Errorf("expected ErrFenced from deposed worker's CompleteJob, got %v", err)
	}

	// B starts and completes the job
	if err := s.StartJob(ctx, jobID, epochB); err != nil {
		t.Fatalf("start B: %v", err)
	}
	err = s.CompleteJob(ctx, jobID, epochB)
	if err != nil {
		t.Fatalf("expected B to complete: %v", err)
	}

	j, err = s.GetJob(ctx, jobID)
	if err != nil || j.ID == uuid.Nil || j.Status != store.StatusCompleted {
		t.Errorf("expected completed, got %v", j)
	}
}

// TestSim_BackoffTiming reproduces U5: a failed job's retry is gated by run_at.
func TestSim_BackoffTiming(t *testing.T) {
	clk := clock.NewManualClock(time.Unix(1700000000, 0))
	s := NewSimStore(clk)

	ctx := context.Background()
	j, err := s.CreateJob(ctx, "segments", []byte(`{"segments":5}`), 0, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := s.ClaimJob(ctx, "worker-a", 150*time.Millisecond)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.StartJob(ctx, j.ID, claimed.LeaseEpoch); err != nil {
		t.Fatalf("start: %v", err)
	}
	jobID := j.ID
	epoch := claimed.LeaseEpoch

	// Fail the job — it gets requeued with run_at = now + backoff
	err = s.FailJob(ctx, jobID, epoch, "simulated failure")
	if err != nil {
		t.Fatalf("FailJob: %v", err)
	}

	// Read the run_at that was set
	j, err = s.GetJob(ctx, jobID)
	if err != nil || j.ID == uuid.Nil || j.RunAt == nil {
		t.Fatal("expected requeued job with run_at")
	}
	runAt := *j.RunAt
	now := clk.Now()
	backoff := runAt.Sub(now)
	if backoff <= 0 {
		t.Errorf("expected run_at in future, got %v", backoff)
	}

	// Advance to just before run_at — job must NOT be claimable
	clk.Advance(backoff - time.Millisecond)

	claimed, err = s.ClaimJob(context.Background(), "test-worker", time.Minute)
	if err != nil || claimed != nil {
		t.Errorf("before run_at: expected no claim, got job=%v err=%v", claimed, err)
	}

	// Advance to exactly run_at — claimability flips
	clk.Advance(time.Millisecond)

	claimed, err = s.ClaimJob(context.Background(), "test-worker", time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("at run_at: expected claim, got job=%v err=%v", claimed, err)
	}
	if claimed.AttemptCount != 2 {
		t.Errorf("expected attempt_count=2 after one failure and re-claim, got %d", claimed.AttemptCount)
	}

	// Start the job before failing it again
	if err := s.StartJob(ctx, claimed.ID, claimed.LeaseEpoch); err != nil {
		t.Fatalf("start after re-claim: %v", err)
	}

	// Verify a second failure follows base*2^1 schedule
	epoch2 := claimed.LeaseEpoch
	err = s.FailJob(ctx, claimed.ID, epoch2, "second failure")
	if err != nil {
		t.Fatalf("second FailJob: %v", err)
	}

	j, err = s.GetJob(ctx, claimed.ID)
	if err != nil || j.ID == uuid.Nil || j.RunAt == nil {
		t.Fatal("expected second requeue with run_at")
	}
	backoff2 := j.RunAt.Sub(clk.Now())
	// base=2s, attempt=1 -> base*2^1 = 4s (plus jitter, cap 5m)
	expectedMin := 4*time.Second - time.Second // 3s to 5s roughly
	expectedMax := 5*time.Minute
	if backoff2 < expectedMin || backoff2 > expectedMax {
		t.Errorf("second backoff=%v out of expected range [%v, %v]", backoff2, expectedMin, expectedMax)
	}
}