package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"forge/internal/llm"
	"forge/internal/store"
	"forge/internal/tools"
)

// memoryStore is an in-memory store.JobStore used to drive the agent loop tests
// deterministically without PostgreSQL.
type memoryStore struct {
	mu    sync.Mutex
	jobs  map[string]*store.Job
	steps map[string][]store.JobStep
	llmCalls map[string][]store.LLMCall
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		jobs:  make(map[string]*store.Job),
		steps: make(map[string][]store.JobStep),
		llmCalls: make(map[string][]store.LLMCall),
	}
}

func (m *memoryStore) CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (store.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := &store.Job{
		ID:         uuid.New(),
		TaskType:   taskType,
		Payload:    payload,
		Status:     store.StatusPending,
		Priority:   priority,
		LeaseEpoch: 1,
	}
	m.jobs[j.ID.String()] = j
	return *j, nil
}

func (m *memoryStore) CountPendingJobs(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int
	for _, j := range m.jobs {
		if j.Status == store.StatusPending {
			count++
		}
	}
	return count, nil
}

func (m *memoryStore) GetJob(ctx context.Context, id uuid.UUID) (store.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id.String()]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return *j, nil
}

func (m *memoryStore) ListJobs(ctx context.Context, status string, limit int) ([]store.Job, error) {
	return nil, nil
}

func (m *memoryStore) ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*store.Job, error) {
	return nil, nil
}

func (m *memoryStore) StartJob(ctx context.Context, id uuid.UUID, epoch int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id.String()]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	j.Status = store.StatusRunning
	return nil
}

func (m *memoryStore) CompleteJob(ctx context.Context, id uuid.UUID, epoch int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id.String()]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	j.Status = store.StatusCompleted
	return nil
}

func (m *memoryStore) FailJob(ctx context.Context, id uuid.UUID, epoch int, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id.String()]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	j.Status = store.StatusFailed
	j.ErrorMessage = &reason
	return nil
}

func (m *memoryStore) RecordStep(ctx context.Context, id uuid.UUID, epoch int, step store.JobStep) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id.String()]
	if !ok {
		return uuid.Nil, store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return uuid.Nil, store.ErrFenced
	}
	step.ID = uuid.New()
	step.JobID = id
	m.steps[id.String()] = append(m.steps[id.String()], step)
	return step.ID, nil
}

func (m *memoryStore) LastCompletedStep(ctx context.Context, id uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	steps := m.steps[id.String()]
	if len(steps) == 0 {
		return 0, nil
	}
	return steps[len(steps)-1].StepNumber, nil
}

func (m *memoryStore) ListSteps(ctx context.Context, id uuid.UUID) ([]store.JobStep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.JobStep(nil), m.steps[id.String()]...), nil
}

func (m *memoryStore) RenewLease(ctx context.Context, id uuid.UUID, epoch int, lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id.String()]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	return nil
}

func (m *memoryStore) Heartbeat(ctx context.Context, workerID, hostname string) error { return nil }

func (m *memoryStore) Ping(ctx context.Context) error { return nil }

func (m *memoryStore) Close() error { return nil }

// countingTool is a deterministic tool that counts its executions, letting the
// thesis test assert that a resumed worker re-runs the tool exactly once.
type countingTool struct {
	calls int
}

func (c *countingTool) Name() string        { return "search_kb" }
func (c *countingTool) Description() string { return "Search KB" }
func (c *countingTool) Schema() string      { return `{"type":"object"}` }
func (c *countingTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	c.calls++
	return fmt.Sprintf("KB search result #%d", c.calls), nil
}

// TestAgentLoop_ResumeMidIterationAfterDepose is the Week-4 thesis test:
//
//	worker-1 records plan step 1, then is DEPOSED (epoch bumped) before it can
//	record the tool_call. worker-2 reclaims (epoch 2), rebuilds the history,
//	detects the pending decision, re-executes the tool WITHOUT re-calling the
//	LLM, then completes the job. Asserts:
//	  - zero LLM re-spend on resume (CallCount stays 2 across both workers)
//	  - tool executes exactly once (by worker-2)
//	  - steps are attributed to 2 distinct worker_ids with complete continuity
func TestAgentLoop_ResumeMidIterationAfterDepose(t *testing.T) {
	ms := newMemoryStore()
	jobVal, err := ms.CreateJob(context.Background(), "cp_solve", json.RawMessage(`{"prompt": "Solve 2-sum"}`), 0, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	job := &jobVal
	job.LeaseEpoch = 1

	cTool := &countingTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(cTool); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	plan1 := `{"thought": "Need KB", "action": "tool", "tool_name": "search_kb", "tool_args": {"query": "two_pointers"}}`
	plan2 := `{"thought": "Found answer", "action": "finish", "answer": "Use two pointers!"}`

	fakeLLM := llm.NewFakeBackend(
		llm.CompleteResponse{Content: plan1},
		llm.CompleteResponse{Content: plan2},
	)

	// Worker-1 runs against a store that depose-fences it right after the first
	// plan row commits â€” the exact kill window the plan is designed around.
	deposingStore := &deposeAfterPlanStore{
		memoryStore: ms,
		targetStep:  1,
	}

	worker1 := New(fakeLLM, reg)
	err = worker1.Run(context.Background(), deposingStore, job, 1, "worker-1")
	if err == nil || !errors.Is(err, store.ErrFenced) {
		t.Fatalf("expected fenced error on worker-1, got: %v", err)
	}

	if fakeLLM.CallCount() != 1 {
		t.Fatalf("expected worker-1 to make exactly 1 LLM call, got %d", fakeLLM.CallCount())
	}

	steps, _ := ms.ListSteps(context.Background(), job.ID)
	if len(steps) != 1 {
		t.Fatalf("expected 1 committed step after depose, got %d", len(steps))
	}
	if steps[0].StepType != StepTypePlan || steps[0].WorkerID != "worker-1" {
		t.Fatalf("unexpected step 1: %+v", steps[0])
	}

	// Worker-2 reclaims: the job's epoch is now 2 (bumped by the depose).
	job.LeaseEpoch = 2

	worker2 := New(fakeLLM, reg)
	err = worker2.Run(context.Background(), ms, job, 2, "worker-2")
	if err != nil {
		t.Fatalf("worker-2 failed to complete the job: %v", err)
	}

	if fakeLLM.CallCount() != 2 {
		t.Fatalf("expected total 2 LLM calls across both workers (ZERO re-spend on worker-2 resume), got %d", fakeLLM.CallCount())
	}
	if cTool.calls != 1 {
		t.Errorf("expected tool to execute exactly once (by worker-2), got %d", cTool.calls)
	}

	finalSteps, _ := ms.ListSteps(context.Background(), job.ID)
	if len(finalSteps) != 3 {
		t.Fatalf("expected 3 final steps (plan, tool_call, plan), got %d", len(finalSteps))
	}

	// Step continuity + dual worker_id attribution.
	if finalSteps[0].StepType != StepTypePlan || finalSteps[0].WorkerID != "worker-1" {
		t.Errorf("expected step 1 plan by worker-1, got %+v", finalSteps[0])
	}
	if finalSteps[1].StepType != StepTypeToolCall || finalSteps[1].WorkerID != "worker-2" {
		t.Errorf("expected step 2 tool_call by worker-2, got %+v", finalSteps[1])
	}
	if finalSteps[2].StepType != StepTypePlan || finalSteps[2].WorkerID != "worker-2" {
		t.Errorf("expected step 3 plan by worker-2, got %+v", finalSteps[2])
	}

	// The nil return means the worker loop would CompleteJob cleanly (not fenced).
	if err := ms.CompleteJob(context.Background(), job.ID, 2); err != nil {
		t.Errorf("job should be completable by worker-2, got: %v", err)
	}
}

// deposeAfterPlanStore records the target step, then bumps the job's epoch and
// returns ErrFenced â€” simulating a concurrent reclaim beating us to the write.
type deposeAfterPlanStore struct {
	*memoryStore
	targetStep int
}

func (d *deposeAfterPlanStore) RecordStep(ctx context.Context, id uuid.UUID, epoch int, step store.JobStep) (uuid.UUID, error) {
	idOut, err := d.memoryStore.RecordStep(ctx, id, epoch, step)
	if err != nil {
		return idOut, err
	}
	if step.StepNumber == d.targetStep {
		d.memoryStore.mu.Lock()
		d.memoryStore.jobs[id.String()].LeaseEpoch++
		d.memoryStore.mu.Unlock()
		return uuid.Nil, store.ErrFenced
	}
	return idOut, nil
}

func TestAgentLoop_MaxStepsFailJob(t *testing.T) {
	ms := newMemoryStore()
	jobVal, _ := ms.CreateJob(context.Background(), "cp_solve", json.RawMessage("infinite tool loop"), 0, "")
	job := &jobVal
	job.LeaseEpoch = 1

	cTool := &countingTool{}
	reg := tools.NewRegistry()
	_ = reg.Register(cTool)

	loopPlan := `{"thought": "Keep looping", "action": "tool", "tool_name": "search_kb", "tool_args": {}}`
	fakeLLM := llm.NewFakeBackend(llm.CompleteResponse{Content: loopPlan})

	t.Setenv("AGENT_MAX_STEPS", "2")
	ag := New(fakeLLM, reg)

	err := ag.Run(context.Background(), ms, job, 1, "worker-1")
	if err == nil {
		t.Fatal("expected max-steps error, got nil")
	}
	if !strings.Contains(err.Error(), "max steps") {
		t.Errorf("expected max steps error, got: %v", err)
	}

	// Like the worker loop, an execution error becomes a FailJob.
	if err := ms.FailJob(context.Background(), job.ID, 1, err.Error()); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	failedJob, _ := ms.GetJob(context.Background(), job.ID)
	if failedJob.Status != store.StatusFailed {
		t.Errorf("expected job status failed, got %s", failedJob.Status)
	}
}

func TestAgentLoop_BadJSONRetriesOnceThenFails(t *testing.T) {
	ms := newMemoryStore()
	jobVal, _ := ms.CreateJob(context.Background(), "cp_solve", json.RawMessage("bad json test"), 0, "")
	job := &jobVal
	job.LeaseEpoch = 1

	fakeLLM := llm.NewFakeBackend(
		llm.CompleteResponse{Content: "invalid json string"},
		llm.CompleteResponse{Content: "still invalid json"},
	)

	ag := New(fakeLLM, nil)
	err := ag.Run(context.Background(), ms, job, 1, "worker-1")
	if err == nil {
		t.Fatal("expected error after bad JSON nudge retry, got nil")
	}
	if fakeLLM.CallCount() != 2 {
		t.Errorf("expected 2 LLM calls (initial + nudge retry), got %d", fakeLLM.CallCount())
	}
}

func TestReconstructHistory_PendingDecisionDetection(t *testing.T) {
	steps := []store.JobStep{
		{
			StepNumber: 1,
			StepType:   StepTypePlan,
			Output:     json.RawMessage(`{"thought":"t","action":"tool","tool_name":"search_kb","tool_args":{"query":"x"}}`),
		},
	}

	messages, pending, iterations, err := reconstructHistory(steps, "sys", "prompt")
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if pending == nil {
		t.Fatal("expected pendingDecision for lone trailing plan row")
	}
	if pending.ToolName != "search_kb" {
		t.Errorf("expected pending tool search_kb, got %s", pending.ToolName)
	}
	if iterations != 1 {
		t.Errorf("expected 1 completed iteration, got %d", iterations)
	}
	if len(messages) != 3 {
		t.Errorf("expected 3 messages (system, user, assistant), got %d", len(messages))
	}
}

func TestReconstructHistory_CompleteIteration(t *testing.T) {
	steps := []store.JobStep{
		{
			StepNumber: 1,
			StepType:   StepTypePlan,
			Output:     json.RawMessage(`{"thought":"t","action":"tool","tool_name":"search_kb","tool_args":{"query":"x"}}`),
		},
		{
			StepNumber: 2,
			StepType:   StepTypeToolCall,
			Output:     json.RawMessage(`"KB result"`),
		},
		{
			StepNumber: 3,
			StepType:   StepTypePlan,
			Output:     json.RawMessage(`{"thought":"t","action":"finish","answer":"done"}`),
		},
	}

	messages, pending, iterations, err := reconstructHistory(steps, "sys", "prompt")
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	// A committed finish plan is reported as a pending finish decision so the
	// caller can return nil without re-calling the LLM.
	if pending == nil || pending.Action != "finish" {
		t.Fatalf("expected pending finish decision, got: %+v", pending)
	}
	if pending.Answer != "done" {
		t.Errorf("expected finish answer 'done', got %q", pending.Answer)
	}
	if iterations != 2 {
		t.Errorf("expected 2 completed iterations, got %d", iterations)
	}
	if len(messages) != 5 {
		t.Errorf("expected 5 messages, got %d", len(messages))
	}
}

func (m *memoryStore) RecordLLMCall(ctx context.Context, call store.LLMCall) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if call.ID == uuid.Nil {
		call.ID = uuid.New()
	}
	if m.llmCalls == nil {
		m.llmCalls = make(map[string][]store.LLMCall)
	}
	m.llmCalls[call.JobID.String()] = append(m.llmCalls[call.JobID.String()], call)
	return call.ID, nil
}

func (m *memoryStore) ListLLMCalls(ctx context.Context, jobID uuid.UUID) ([]store.LLMCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.llmCalls == nil {
		return []store.LLMCall{}, nil
	}
	calls := m.llmCalls[jobID.String()]
	if calls == nil {
		calls = []store.LLMCall{}
	}
	return append([]store.LLMCall(nil), calls...), nil
}


func TestCompleteAndRecord(t *testing.T) {
	ms := newMemoryStore()
	job, _ := ms.CreateJob(context.Background(), "cp_solve", json.RawMessage("{}"), 0, "")

	fakeLLM := llm.NewFakeBackend(llm.CompleteResponse{
		Content: "hello",
		Usage:   llm.Usage{PromptTokens: 10, CompletionTokens: 5},
	})
	ag := New(fakeLLM, tools.NewRegistry())

	req := llm.CompleteRequest{
		Messages: []llm.Message{{Role: "user", Content: "test"}},
	}

	resp, err := ag.completeAndRecord(context.Background(), ms, job.ID, "w-123", req)
	if err != nil {
		t.Fatalf("completeAndRecord failed: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("expected 'hello', got %q", resp.Content)
	}

	calls, err := ms.ListLLMCalls(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("ListLLMCalls failed: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 LLM call recorded, got %d", len(calls))
	}
	if calls[0].PromptTokens != 10 || calls[0].CompletionTokens != 5 {
		t.Errorf("unexpected tokens: %+v", calls[0])
	}
	if calls[0].WorkerID == nil || *calls[0].WorkerID != "w-123" {
		t.Errorf("expected workerID 'w-123', got %v", calls[0].WorkerID)
	}
}
