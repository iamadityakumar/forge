package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"forge/internal/testutil"
)

func getTestStore(t *testing.T) (*PgStore, func()) {
	t.Helper()
	// Integration tests run against a dedicated per-package test database
	// (forge_test_store), created and migrated on demand, so they never share —
	// or truncate — the live `forge` database that the running worker stack uses.
	dbURL := testutil.PrepareTestDB(t, "store")

	store, err := NewPgStore(dbURL)
	if err != nil {
		t.Skipf("Skipping store integration test: database connection failed: %v", err)
	}

	// Clean tables before starting
	_, err = store.DB().Exec("TRUNCATE TABLE jobs CASCADE; TRUNCATE TABLE workers CASCADE;")
	if err != nil {
		t.Fatalf("Failed to truncate tables: %v", err)
	}

	cleanup := func() {
		_, _ = store.DB().Exec("TRUNCATE TABLE jobs CASCADE; TRUNCATE TABLE workers CASCADE;")
		store.Close()
	}

	return store, cleanup
}

func TestPgStore_CreateAndGet(t *testing.T) {
	s, cleanup := getTestStore(t)
	defer cleanup()

	ctx := context.Background()
	payload := json.RawMessage(`{"input":"test"}`)
	job, err := s.CreateJob(ctx, "test-task", payload, 10, "idem-key-1")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	if job.TaskType != "test-task" {
		t.Errorf("Expected task type test-task, got %s", job.TaskType)
	}
	var expectedPayload, actualPayload map[string]any
	if err := json.Unmarshal([]byte(`{"input":"test"}`), &expectedPayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(job.Payload, &actualPayload); err != nil {
		t.Fatal(err)
	}
	if actualPayload["input"] != expectedPayload["input"] {
		t.Errorf("Expected payload input %v, got %v", expectedPayload["input"], actualPayload["input"])
	}
	if job.Priority != 10 {
		t.Errorf("Expected priority 10, got %d", job.Priority)
	}
	if job.IdempotencyKey == nil || *job.IdempotencyKey != "idem-key-1" {
		t.Errorf("Expected idempotency key 'idem-key-1', got %v", job.IdempotencyKey)
	}

	// Test idempotency: inserting again with same key returns the same job
	dupJob, err := s.CreateJob(ctx, "test-task-diff", json.RawMessage(`{}`), 0, "idem-key-1")
	if err != nil {
		t.Fatalf("Failed to create duplicate job: %v", err)
	}
	if dupJob.ID != job.ID {
		t.Errorf("Expected duplicate job to return same ID %s, got %s", job.ID, dupJob.ID)
	}

	// Fetch job
	fetched, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if fetched.ID != job.ID {
		t.Errorf("Expected fetched job ID %s, got %s", job.ID, fetched.ID)
	}
}

func TestPgStore_StateTransitions(t *testing.T) {
	s, cleanup := getTestStore(t)
	defer cleanup()

	ctx := context.Background()
	job, err := s.CreateJob(ctx, "test-task", nil, 0, "")
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Verify initial status
	if job.Status != StatusPending {
		t.Errorf("Expected status %s, got %s", StatusPending, job.Status)
	}

	// Attempting to StartJob directly should fail (must be claimed first).
	// epoch is 0 (the job's initial lease_epoch); status is 'pending', so the
	// fenced write matches the epoch but the wrong status → ErrInvalidTransition
	// (NOT ErrFenced, which would require an epoch mismatch).
	err = s.StartJob(ctx, job.ID, 0)
	if err == nil {
		t.Error("Expected error when starting job without claiming it first")
	}

	// Claim the job (ClaimJob mints a new fencing token: lease_epoch 0 → 1).
	claimed, err := s.ClaimJob(ctx, "worker-1", 10*time.Second)
	if err != nil {
		t.Fatalf("Failed to claim job: %v", err)
	}
	if claimed == nil {
		t.Fatal("Expected job to be claimed, got nil")
	}
	if claimed.ID != job.ID {
		t.Errorf("Expected claimed job ID %s, got %s", job.ID, claimed.ID)
	}
	if claimed.Status != StatusClaimed {
		t.Errorf("Expected status %s, got %s", StatusClaimed, claimed.Status)
	}
	if claimed.LeaseEpoch != 1 {
		t.Errorf("Expected ClaimJob to mint lease_epoch=1, got %d", claimed.LeaseEpoch)
	}

	// Start the job using the fencing token ClaimJob minted.
	err = s.StartJob(ctx, job.ID, claimed.LeaseEpoch)
	if err != nil {
		t.Fatalf("Failed to start job: %v", err)
	}

	fetched, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if fetched.Status != StatusRunning {
		t.Errorf("Expected status %s, got %s", StatusRunning, fetched.Status)
	}

	// Fail the job, fenced by the same epoch. With Phase 4, the first failure
	// (attempt_count 1 < default max_attempts 3) REQUEUES the job back to
	// 'pending' with a future run_at (retry with backoff), rather than
	// dead-ending into 'failed'.
	err = s.FailJob(ctx, job.ID, claimed.LeaseEpoch, "some failure reason")
	if err != nil {
		t.Fatalf("Failed to fail job: %v", err)
	}

	fetched, err = s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if fetched.Status != StatusPending {
		t.Errorf("Expected requeued status %s (attempt 1 of 3 requeues), got %s", StatusPending, fetched.Status)
	}
	if fetched.RunAt == nil {
		t.Error("Expected requeued job to have a future run_at (retry backoff)")
	} else if fetched.RunAt.Before(time.Now()) {
		t.Errorf("Expected run_at in the future, got %s", fetched.RunAt)
	}
	if fetched.ErrorMessage == nil || *fetched.ErrorMessage != "some failure reason" {
		t.Errorf("Expected error message 'some failure reason', got %v", fetched.ErrorMessage)
	}
	if fetched.DeadLetter {
		t.Error("Expected first failure to NOT be dead-lettered")
	}
}

func TestPgStore_ClaimJobConcurrent(t *testing.T) {
	s, cleanup := getTestStore(t)
	defer cleanup()

	ctx := context.Background()
	numJobs := 10

	// Create 10 pending jobs
	for i := 0; i < numJobs; i++ {
		_, err := s.CreateJob(ctx, "test-task", nil, 0, "")
		if err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
	}

	// Start 10 goroutines to claim jobs concurrently
	var wg sync.WaitGroup
	claimedJobs := make(chan uuid.UUID, numJobs)
	errChan := make(chan error, numJobs)

	for i := 0; i < numJobs; i++ {
		wg.Add(1)
		go func(workerNum int) {
			defer wg.Done()
			workerID := uuid.New().String()
			job, err := s.ClaimJob(ctx, workerID, 2*time.Minute)
			if err != nil {
				errChan <- err
				return
			}
			if job != nil {
				claimedJobs <- job.ID
			}
		}(i)
	}

	wg.Wait()
	close(claimedJobs)
	close(errChan)

	// Check if any errors occurred
	for err := range errChan {
		t.Errorf("ClaimJob error: %v", err)
	}

	// Gather all unique claimed IDs
	claimedMap := make(map[uuid.UUID]bool)
	for id := range claimedJobs {
		if claimedMap[id] {
			t.Errorf("Job %s claimed multiple times!", id)
		}
		claimedMap[id] = true
	}

	if len(claimedMap) != numJobs {
		t.Errorf("Expected %d jobs to be claimed, but only %d were claimed", numJobs, len(claimedMap))
	}
}

// TestPgStore_FencingAndReclaimRunning proves the two core Phase-1 upgrades:
//
//   - U3 (reclaim running): a job left in 'running' with an expired lease is
//     reclaimed by another worker (the textbook query that only reclaims
//     'claimed' loses this job forever).
//   - U1 (fencing tokens): after B reclaims, a deposed worker A that thaws and
//     tries to complete the job with its STALE epoch affects zero rows and gets
//     ErrFenced — it cannot complete a job B now owns. Double execution is
//     prevented by construction.
func TestPgStore_FencingAndReclaimRunning(t *testing.T) {
	s, cleanup := getTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Worker A claims a job; ClaimJob mints a fencing token (lease_epoch 0 -> 1)
	// that A must present on every subsequent fenced write.
	job, err := s.CreateJob(ctx, "test-task", nil, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	claimedA, err := s.ClaimJob(ctx, "wrk-a", 2*time.Minute)
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if claimedA == nil {
		t.Fatal("expected A to claim the job")
	}
	epochA := claimedA.LeaseEpoch
	if epochA != 1 {
		t.Fatalf("expected first claim to mint lease_epoch=1, got %d", epochA)
	}

	// A starts the job (claimed -> running). A crash here leaves the row in
	// 'running' — the exact state the textbook query (which only reclaims
	// 'claimed') can never pick up again.
	if err := s.StartJob(ctx, job.ID, epochA); err != nil {
		t.Fatalf("start A: %v", err)
	}

	// Force A's lease to expire (simulating a frozen/killed worker) without
	// A ever reaching CompleteJob. Deterministic, no real-time sleep.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`,
		job.ID,
	); err != nil {
		t.Fatalf("force lease expiry: %v", err)
	}

	// Worker B reclaims the EXPIRED 'running' job (U3). The claim subselect now
	// matches 'running'; epoch increments 1 -> 2.
	claimedB, err := s.ClaimJob(ctx, "wrk-b", 2*time.Minute)
	if err != nil {
		t.Fatalf("claim B: %v", err)
	}
	if claimedB == nil {
		t.Fatal("expected B to reclaim the expired running job (reclaim-running / U3)")
	}
	if claimedB.ID != job.ID {
		t.Fatalf("expected B to reclaim the same job, got %v", claimedB.ID)
	}
	if claimedB.LeaseEpoch != epochA+1 {
		t.Errorf("expected reclaim to bump lease_epoch to %d, got %d", epochA+1, claimedB.LeaseEpoch)
	}

	// B starts the reclaimed job (claimed -> running) under its fresh epoch.
	if err := s.StartJob(ctx, job.ID, claimedB.LeaseEpoch); err != nil {
		t.Fatalf("start B: %v", err)
	}

	// A thaws and tries to complete the job it still believes it owns, using its
	// STALE fencing token. This is the zombie/fencing race (U1): A's fenced
	// CompleteJob must affect ZERO rows and return ErrFenced — it must not be
	// able to complete a job B now owns.
	if err := s.CompleteJob(ctx, job.ID, epochA); !errors.Is(err, ErrFenced) {
		t.Fatalf("expected ErrFenced from a deposed worker's CompleteJob, got %v", err)
	}

	// B legitimately completes the job with its fresh epoch.
	if err := s.CompleteJob(ctx, job.ID, claimedB.LeaseEpoch); err != nil {
		t.Fatalf("expected B to complete the job, got %v", err)
	}
	final, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Errorf("expected final status completed, got %s", final.Status)
	}
}

// TestPgStore_RetryAndDeadLetter proves U5 end to end: a job that keeps failing
// is requeued to 'pending' with a future run_at that the claim gate respects
// (ClaimJob returns nil until run_at elapses) until attempt_count reaches
// max_attempts, after which FailJob dead-letters it and it surfaces via
// ListJobs(status="dead_letter") while never being reclaimable again.
func TestPgStore_RetryAndDeadLetter(t *testing.T) {
	s, cleanup := getTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Tighten backoff so retry delays stay small; we still fast-forward run_at
	// past between attempts to keep the test deterministic and fast.
	prevBase := backoffBase
	backoffBase = 100 * time.Millisecond
	defer func() { backoffBase = prevBase }()

	job, err := s.CreateJob(ctx, "poison", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const maxAttempts = 3 // default max_attempts
	var epoch int

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// After a requeue the job is 'pending' with run_at in the future, so
		// model "the backoff elapsed" by moving run_at into the past.
		if attempt > 1 {
			if _, err := s.DB().ExecContext(ctx,
				`UPDATE jobs SET run_at = now() - interval '1 second' WHERE id = $1`, job.ID,
			); err != nil {
				t.Fatalf("fast-forward run_at attempt %d: %v", attempt, err)
			}
		}
		claimed, err := s.ClaimJob(ctx, "wrk", time.Minute)
		if err != nil || claimed == nil || claimed.ID != job.ID {
			t.Fatalf("claim before fail %d: job=%v err=%v", attempt, claimed, err)
		}
		if claimed.AttemptCount != attempt {
			t.Errorf("claim %d: attempt_count got %d want %d", attempt, claimed.AttemptCount, attempt)
		}
		epoch = claimed.LeaseEpoch
		if err := s.StartJob(ctx, job.ID, epoch); err != nil {
			t.Fatalf("start %d: %v", attempt, err)
		}
		if err := s.FailJob(ctx, job.ID, epoch, "boom"); err != nil {
			t.Fatalf("fail %d: %v", attempt, err)
		}

		got, _ := s.GetJob(ctx, job.ID)
		if attempt < maxAttempts {
			if got.Status != StatusPending {
				t.Errorf("after fail %d: want status pending (requeued), got %s", attempt, got.Status)
			}
			if got.DeadLetter {
				t.Errorf("after fail %d: should not be dead-lettered yet", attempt)
			}
			if got.RunAt == nil || got.RunAt.Before(time.Now()) {
				t.Errorf("after fail %d: run_at should be in the future, got %v", attempt, got.RunAt)
			}
			// The run_at gate must hold: the job is not reclaimable until elapsed.
			if stolen, err := s.ClaimJob(ctx, "wrk2", time.Minute); err != nil || stolen != nil {
				t.Errorf("after requeue fail %d: ClaimJob should return nil (run_at in future), got job=%v err=%v", attempt, stolen, err)
			}
		} else {
			if got.Status != StatusFailed {
				t.Errorf("after fail %d: want status failed, got %s", attempt, got.Status)
			}
			if !got.DeadLetter {
				t.Errorf("after fail %d: expected dead_letter=true", attempt)
			}
			if got.ErrorMessage == nil || *got.ErrorMessage != "boom" {
				t.Errorf("after fail %d: error_message got %v want 'boom'", attempt, got.ErrorMessage)
			}
			if got.CompletedAt == nil {
				t.Errorf("after fail %d: expected completed_at set (dead-letter is terminal)", attempt)
			}
		}
	}

	// A dead-lettered job must never be reclaimed (no record of it is claimable).
	if stolen, err := s.ClaimJob(ctx, "wrk-late", time.Minute); err != nil || stolen != nil {
		t.Errorf("dead-lettered job should never be reclaimed: got job=%v err=%v", stolen, err)
	}
	// And it must surface via the virtual dead_letter filter.
	dl, err := s.ListJobs(ctx, ListJobsOpts{Status: StatusDeadLetter, Limit: 50})
	if err != nil {
		t.Fatalf("list dead_letter: %v", err)
	}
	if len(dl) != 1 || dl[0].ID != job.ID {
		t.Errorf("expected exactly this job in dead_letter list, got %d entries", len(dl))
	}
}
