package store

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func getTestStore(t *testing.T) (*PgStore, func()) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		// Fallback to local docker compose postgres for convenience during local runs
		dbURL = "postgres://postgres:secret@localhost:5432/forge?sslmode=disable"
	}

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
	if string(job.Payload) != `{"input":"test"}` {
		t.Errorf("Expected payload %s, got %s", `{"input":"test"}`, string(job.Payload))
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

	// Attempting to StartJob directly should fail (must be claimed first)
	err = s.StartJob(ctx, job.ID)
	if err == nil {
		t.Error("Expected error when starting job without claiming it first")
	}

	// Claim the job
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

	// Start the job
	err = s.StartJob(ctx, job.ID)
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

	// Fail the job
	err = s.FailJob(ctx, job.ID, "some failure reason")
	if err != nil {
		t.Fatalf("Failed to fail job: %v", err)
	}

	fetched, err = s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if fetched.Status != StatusFailed {
		t.Errorf("Expected status %s, got %s", StatusFailed, fetched.Status)
	}
	if fetched.ErrorMessage == nil || *fetched.ErrorMessage != "some failure reason" {
		t.Errorf("Expected error message 'some failure reason', got %v", fetched.ErrorMessage)
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
