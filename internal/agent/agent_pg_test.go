package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"forge/internal/llm"
	"forge/internal/store"
	"forge/internal/testutil"
	"forge/internal/tools"
)

// getTestAgentStore opens a real PgStore on the dedicated per-package test
// database (forge_test_agent) and truncates the jobs/workers tables, skipping
// the test when no database is reachable (same gating as the store package's
// integration tests).
func getTestAgentStore(t *testing.T) (*store.PgStore, func()) {
	t.Helper()
	// Dedicated per-package test database (forge_test_agent): the agent smoke
	// test must not race live workers on the shared `forge` database.
	dbURL := testutil.PrepareTestDB(t, "agent")

	s, err := store.NewPgStore(dbURL)
	if err != nil {
		t.Skipf("Skipping agent Postgres smoke test: database connection failed: %v", err)
	}

	if _, err := s.DB().Exec("TRUNCATE TABLE jobs CASCADE; TRUNCATE TABLE workers CASCADE;"); err != nil {
		t.Fatalf("failed to truncate tables: %v", err)
	}

	cleanup := func() {
		_, _ = s.DB().Exec("TRUNCATE TABLE jobs CASCADE; TRUNCATE TABLE workers CASCADE;")
		s.Close()
	}
	return s, cleanup
}

// TestAgentLoop_RealStoreSmoke verifies the full agent loop against real
// PostgreSQL: job insertion, epoch-fenced step insertion with per-step worker_id
// attribution, and a clean finish transition.
func TestAgentLoop_RealStoreSmoke(t *testing.T) {
	s, cleanup := getTestAgentStore(t)
	defer cleanup()

	ctx := context.Background()

	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewSearchKBTool()); err != nil {
		t.Fatalf("register search_kb: %v", err)
	}

	finishPlan := `{"thought":"solution ready","action":"finish","answer":"Use a two-pointer approach."}`
	fakeLLM := llm.NewFakeBackend(
		llm.CompleteResponse{Content: finishPlan},
	)

	job, err := s.ClaimJob(ctx, "smoke-worker", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job != nil {
		// A job was already claimable (shouldn't be — we truncated); don't pollute.
		t.Fatalf("expected empty queue, got job %s", job.ID)
	}

	created, err := s.CreateJob(ctx, "cp_solve", json.RawMessage(`{"prompt":"Solve two-sum in O(n)"}`), 0, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	claimed, err := s.ClaimJob(ctx, "smoke-worker", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != created.ID {
		t.Fatalf("expected to claim created job, got %+v", claimed)
	}
	if err := s.StartJob(ctx, claimed.ID, claimed.LeaseEpoch); err != nil {
		t.Fatalf("start job: %v", err)
	}

	ag := New(fakeLLM, reg, nil)
	err = ag.Run(ctx, s, claimed, claimed.LeaseEpoch, "smoke-worker")
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}

	// The nil return means the worker loop can complete the job cleanly.
	if err := s.CompleteJob(ctx, claimed.ID, claimed.LeaseEpoch); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	final, err := s.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if final.Status != store.StatusCompleted {
		t.Errorf("expected completed status, got %s", final.Status)
	}

	steps, err := s.ListSteps(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 plan step, got %d", len(steps))
	}
	if steps[0].StepType != StepTypePlan || steps[0].StepNumber != 1 {
		t.Errorf("unexpected step: %+v", steps[0])
	}
	if steps[0].WorkerID != "smoke-worker" {
		t.Errorf("expected worker_id attribution 'smoke-worker', got %q", steps[0].WorkerID)
	}

	var decision Decision
	if err := json.Unmarshal(steps[0].Output, &decision); err != nil {
		t.Fatalf("step output should be decision JSON, got: %v", err)
	}
	if decision.Action != "finish" || decision.Answer == "" {
		t.Errorf("unexpected decision: %+v", decision)
	}
}
