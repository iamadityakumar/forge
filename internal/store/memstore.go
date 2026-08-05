package store

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is a thread-safe in-memory implementation of JobStore.
type MemStore struct {
	mu          sync.Mutex
	jobs        map[uuid.UUID]Job
	steps       map[uuid.UUID][]JobStep
	llmCalls    map[uuid.UUID][]LLMCall
	workerHeart map[string]time.Time
}

// NewMemStore creates and initializes a MemStore pre-seeded with sample data for demonstration.
func NewMemStore() *MemStore {
	m := &MemStore{
		jobs:        make(map[uuid.UUID]Job),
		steps:       make(map[uuid.UUID][]JobStep),
		llmCalls:    make(map[uuid.UUID][]LLMCall),
		workerHeart: make(map[string]time.Time),
	}

	now := time.Now().UTC()
	m.workerHeart["worker-1"] = now
	m.workerHeart["worker-2"] = now
	m.workerHeart["worker-3"] = now
	m.workerHeart["worker-4"] = now

	// Seed Sample Job 1 (Completed with cross-worker trace)
	id1 := uuid.New()
	j1 := Job{
		ID:          id1,
		TaskType:    "cp_solve",
		Payload:     json.RawMessage(`{"problem_id":"cp_101"}`),
		Status:      StatusCompleted,
		Priority:    1,
		ClaimedBy:   strPtr("worker-3"),
		MaxAttempts: 3,
		CreatedAt:   now.Add(-5 * time.Minute),
	}
	m.jobs[id1] = j1

	m.steps[id1] = []JobStep{
		{ID: uuid.New(), JobID: id1, StepNumber: 1, StepType: "plan", Status: StepCompleted, WorkerID: "worker-1", Output: json.RawMessage(`{"action":"tool","name":"search_kb"}`), DurationMs: 320, CreatedAt: now.Add(-4 * time.Minute)},
		{ID: uuid.New(), JobID: id1, StepNumber: 2, StepType: "tool_call", Status: StepCompleted, WorkerID: "worker-1", Output: json.RawMessage(`{"found":true}`), DurationMs: 140, CreatedAt: now.Add(-3 * time.Minute)},
		{ID: uuid.New(), JobID: id1, StepNumber: 3, StepType: "plan", Status: StepCompleted, WorkerID: "worker-3", Output: json.RawMessage(`{"action":"tool","name":"run_tests"}`), DurationMs: 410, CreatedAt: now.Add(-2 * time.Minute)},
		{ID: uuid.New(), JobID: id1, StepNumber: 4, StepType: "tool_call", Status: StepCompleted, WorkerID: "worker-3", Output: json.RawMessage(`{"passed":true}`), DurationMs: 890, CreatedAt: now.Add(-2 * time.Minute)},
		{ID: uuid.New(), JobID: id1, StepNumber: 5, StepType: "plan", Status: StepCompleted, WorkerID: "worker-3", Output: json.RawMessage(`{"action":"finish"}`), DurationMs: 210, CreatedAt: now.Add(-2 * time.Minute)},
	}

	// Seed Sample Job 2 (Running)
	id2 := uuid.New()
	j2 := Job{
		ID:          id2,
		TaskType:    "segment_process",
		Payload:     json.RawMessage(`{"segment_id":42}`),
		Status:      StatusRunning,
		Priority:    0,
		ClaimedBy:   strPtr("worker-2"),
		MaxAttempts: 3,
		CreatedAt:   now.Add(-1 * time.Minute),
	}
	m.jobs[id2] = j2

	m.steps[id2] = []JobStep{
		{ID: uuid.New(), JobID: id2, StepNumber: 1, StepType: "segment", Status: StepCompleted, WorkerID: "worker-2", Output: json.RawMessage(`{"processed":true}`), DurationMs: 180, CreatedAt: now.Add(-30 * time.Second)},
	}

	return m
}

func (m *MemStore) CreateJob(_ context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if idempotencyKey != "" {
		for _, j := range m.jobs {
			if j.IdempotencyKey != nil && *j.IdempotencyKey == idempotencyKey {
				return j, nil
			}
		}
	}
	var idemKey *string
	if idempotencyKey != "" {
		idemKey = &idempotencyKey
	}
	j := Job{
		ID:             uuid.New(),
		TaskType:       taskType,
		Payload:        payload,
		Status:         StatusPending,
		Priority:       priority,
		IdempotencyKey: idemKey,
		CreatedAt:      time.Now().UTC(),
		MaxAttempts:    3,
	}
	m.jobs[j.ID] = j
	return j, nil
}

func (m *MemStore) GetJob(_ context.Context, id uuid.UUID) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return j, nil
}

func (m *MemStore) ListJobs(_ context.Context, status string, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Job
	for _, j := range m.jobs {
		if status == "" || status == StatusDeadLetter || j.Status == status {
			out = append(out, j)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MemStore) CountPendingJobs(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var count int
	for _, j := range m.jobs {
		if j.Status == StatusPending {
			count++
		}
	}
	return count, nil
}

func (m *MemStore) ClaimJob(_ context.Context, _ string, _ time.Duration) (*Job, error) {
	return nil, nil
}

func (m *MemStore) StartJob(_ context.Context, _ uuid.UUID, _ int) error   { return nil }
func (m *MemStore) CompleteJob(_ context.Context, _ uuid.UUID, _ int) error { return nil }
func (m *MemStore) FailJob(_ context.Context, _ uuid.UUID, _ int, _ string) error {
	return nil
}

func (m *MemStore) RecordStep(_ context.Context, jobID uuid.UUID, epoch int, step JobStep) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if step.ID == uuid.Nil {
		step.ID = uuid.New()
	}
	step.JobID = jobID
	m.steps[jobID] = append(m.steps[jobID], step)
	return step.ID, nil
}

func (m *MemStore) LastCompletedStep(_ context.Context, jobID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	steps := m.steps[jobID]
	maxStep := 0
	for _, s := range steps {
		if s.Status == StepCompleted && s.StepNumber > maxStep {
			maxStep = s.StepNumber
		}
	}
	return maxStep, nil
}

func (m *MemStore) ListSteps(_ context.Context, jobID uuid.UUID) ([]JobStep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.steps[jobID], nil
}

func (m *MemStore) RecordLLMCall(_ context.Context, call LLMCall) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if call.ID == uuid.Nil {
		call.ID = uuid.New()
	}
	m.llmCalls[call.JobID] = append(m.llmCalls[call.JobID], call)
	return call.ID, nil
}

func (m *MemStore) ListLLMCalls(_ context.Context, jobID uuid.UUID) ([]LLMCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.llmCalls[jobID], nil
}

func (m *MemStore) RenewLease(_ context.Context, _ uuid.UUID, _ int, _ time.Duration) error {
	return nil
}

func (m *MemStore) Heartbeat(_ context.Context, workerID string, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.workerHeart[workerID] = time.Now().UTC()
	return nil
}

func (m *MemStore) CountActiveWorkers(_ context.Context, within time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().UTC().Add(-within)
	count := 0
	for id, t := range m.workerHeart {
		if t.After(cutoff) {
			count++
		} else if id == "worker-1" || id == "worker-2" || id == "worker-3" || id == "worker-4" {
			m.workerHeart[id] = time.Now().UTC()
			count++
		}
	}
	return count, nil
}

func (m *MemStore) SetTraceContext(_ context.Context, _ uuid.UUID, _ int, _ json.RawMessage) error {
	return nil
}

func (m *MemStore) Ping(_ context.Context) error { return nil }
func (m *MemStore) Close() error                 { return nil }

func strPtr(s string) *string {
	return &s
}
