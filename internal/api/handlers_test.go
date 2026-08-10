package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"forge/internal/store"
)

// memStore is a minimal in-memory JobStore used for handler unit tests.
// It avoids the need for a running Postgres in CI for basic HTTP tests.
type memStore struct {
	jobs     map[uuid.UUID]store.Job
	steps    map[uuid.UUID][]store.JobStep
	llmCalls map[uuid.UUID][]store.LLMCall
}

func newMemStore() *memStore {
	return &memStore{
		jobs:     make(map[uuid.UUID]store.Job),
		steps:    make(map[uuid.UUID][]store.JobStep),
		llmCalls: make(map[uuid.UUID][]store.LLMCall),
	}
}

func (m *memStore) CreateJob(_ context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (store.Job, error) {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	// Check idempotency.
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
	j := store.Job{
		ID:             uuid.New(),
		TaskType:       taskType,
		Payload:        payload,
		Status:         store.StatusPending,
		Priority:       priority,
		IdempotencyKey: idemKey,
		CreatedAt:      time.Now().UTC(),
		MaxAttempts:    3,
	}
	m.jobs[j.ID] = j
	return j, nil
}

func (m *memStore) CountActiveWorkers(_ context.Context, _ time.Duration) (int, error) { return 1, nil }

func (m *memStore) SetTraceContext(_ context.Context, _ uuid.UUID, _ int, _ json.RawMessage) error { return nil }

func (m *memStore) CountPendingJobs(_ context.Context) (int, error) {
	var count int
	for _, j := range m.jobs {
		if j.Status == store.StatusPending {
			count++
		}
	}
	return count, nil
}

func (m *memStore) CountJobs(_ context.Context) (store.JobCounts, error) {
	var c store.JobCounts
	for _, j := range m.jobs {
		c.Total++
		switch j.Status {
		case store.StatusPending:
			c.Pending++
		case store.StatusClaimed, store.StatusRunning:
			c.Running++
		case store.StatusCompleted:
			c.Completed++
		case store.StatusFailed:
			c.Failed++
		}
		if j.DeadLetter {
			c.DeadLetter++
		}
	}
	return c, nil
}

func (m *memStore) GetJob(_ context.Context, id uuid.UUID) (store.Job, error) {
	j, ok := m.jobs[id]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return j, nil
}

func (m *memStore) ListJobs(_ context.Context, opts store.ListJobsOpts) ([]store.Job, error) {
	var out []store.Job
	for _, j := range m.jobs {
		if opts.Status != "" {
			if opts.Status == store.StatusDeadLetter {
				// status=="dead_letter" is a virtual filter; in-memory mock has no dead-letter jobs
				continue
			}
			if j.Status != opts.Status {
				continue
			}
		}
		if opts.TaskType != "" && j.TaskType != opts.TaskType {
			continue
		}
		if opts.WorkerID != "" {
			if j.ClaimedBy == nil || *j.ClaimedBy != opts.WorkerID {
				continue
			}
		}
		if opts.Since != nil && j.CreatedAt.Before(*opts.Since) {
			continue
		}
		out = append(out, j)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	// Apply offset
	if opts.Offset > 0 && opts.Offset < len(out) {
		out = out[opts.Offset:]
	} else if opts.Offset >= len(out) {
		out = []store.Job{}
	}
	return out, nil
}

func (m *memStore) ClaimJob(_ context.Context, _ string, _ time.Duration) (*store.Job, error) {
	return nil, nil
}
func (m *memStore) StartJob(_ context.Context, _ uuid.UUID, _ int) error            { return nil }
func (m *memStore) CompleteJob(_ context.Context, _ uuid.UUID, _ int) error          { return nil }
func (m *memStore) FailJob(_ context.Context, _ uuid.UUID, _ int, _ string) error    { return nil }
func (m *memStore) RecordStep(_ context.Context, _ uuid.UUID, _ int, _ store.JobStep) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (m *memStore) LastCompletedStep(_ context.Context, _ uuid.UUID) (int, error) { return 0, nil }
func (m *memStore) ListSteps(_ context.Context, id uuid.UUID) ([]store.JobStep, error) {
	return m.steps[id], nil
}
func (m *memStore) RecordLLMCall(_ context.Context, call store.LLMCall) (uuid.UUID, error) {
	if call.ID == uuid.Nil {
		call.ID = uuid.New()
	}
	m.llmCalls[call.JobID] = append(m.llmCalls[call.JobID], call)
	return call.ID, nil
}
func (m *memStore) ListLLMCalls(_ context.Context, jobID uuid.UUID) ([]store.LLMCall, error) {
	return m.llmCalls[jobID], nil
}
func (m *memStore) RenewLease(_ context.Context, _ uuid.UUID, _ int, _ time.Duration) error { return nil }
func (m *memStore) Heartbeat(_ context.Context, _ string, _ string) error  { return nil }
func (m *memStore) Ping(_ context.Context) error                           { return nil }
func (m *memStore) Close() error                                           { return nil }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func newTestRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func TestCreateJob(t *testing.T) {
	h := NewHandler(newMemStore())
	ts := httptest.NewServer(newTestRouter(h))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/jobs", "application/json",
		strings.NewReader(`{"task_type":"ping","payload":{"k":"v"}}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want status %d, got %d", http.StatusCreated, resp.StatusCode)
	}

	var job store.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID == uuid.Nil {
		t.Fatal("expected non-nil job ID")
	}
	if job.Status != store.StatusPending {
		t.Fatalf("want status %q, got %q", store.StatusPending, job.Status)
	}
}

func TestCreateJobMissingTaskType(t *testing.T) {
	h := NewHandler(newMemStore())
	ts := httptest.NewServer(newTestRouter(h))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/jobs", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want status %d, got %d", http.StatusBadRequest, resp.StatusCode)
	}
}

func TestGetJob(t *testing.T) {
	ms := newMemStore()
	h := NewHandler(ms)

	created, _ := ms.CreateJob(context.Background(), "ping", json.RawMessage(`{}`), 0, "")

	req := httptest.NewRequest(http.MethodGet, "/jobs/"+created.ID.String(), nil)
	rr := httptest.NewRecorder()

	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, got %d", http.StatusOK, rr.Code)
	}

	var job store.Job
	if err := json.NewDecoder(rr.Body).Decode(&job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID != created.ID {
		t.Fatalf("want id %q, got %q", created.ID, job.ID)
	}
}

func TestGetJobNotFound(t *testing.T) {
	h := NewHandler(newMemStore())

	fakeID := uuid.New().String()
	req := httptest.NewRequest(http.MethodGet, "/jobs/"+fakeID, nil)
	rr := httptest.NewRecorder()

	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want status %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestListJobs(t *testing.T) {
	ms := newMemStore()
	h := NewHandler(ms)

	ms.CreateJob(context.Background(), "a", json.RawMessage(`{}`), 0, "")
	ms.CreateJob(context.Background(), "b", json.RawMessage(`{}`), 0, "")

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr := httptest.NewRecorder()

	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, got %d", http.StatusOK, rr.Code)
	}

	var jobs []store.Job
	if err := json.NewDecoder(rr.Body).Decode(&jobs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(jobs))
	}
}

func TestHealthEndpoint(t *testing.T) {
	h := NewHandler(newMemStore())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()

	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, got %d", http.StatusOK, rr.Code)
	}
}

func TestJobTrace(t *testing.T) {
	ms := newMemStore()
	h := NewHandler(ms)

	created, _ := ms.CreateJob(context.Background(), "segments", json.RawMessage(`{}`), 0, "")
	// Seed two checkpointed steps (the trace endpoint returns them ordered).
	now := time.Now().UTC()
	ms.steps[created.ID] = []store.JobStep{
		{ID: uuid.New(), JobID: created.ID, StepNumber: 1, StepType: "segment", Status: store.StepCompleted, CreatedAt: now},
		{ID: uuid.New(), JobID: created.ID, StepNumber: 2, StepType: "segment", Status: store.StepCompleted, CreatedAt: now},
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs/"+created.ID.String()+"/trace", nil)
	rr := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, got %d", http.StatusOK, rr.Code)
	}
	var steps []store.JobStep
	if err := json.NewDecoder(rr.Body).Decode(&steps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	for i, st := range steps {
		if st.StepNumber != i+1 {
			t.Errorf("steps not ordered: pos %d has step %d", i, st.StepNumber)
		}
		if st.JobID != created.ID {
			t.Errorf("step %d job_id %v, want %v", i, st.JobID, created.ID)
		}
	}
}

func TestJobTraceNotFound(t *testing.T) {
	h := NewHandler(newMemStore())

	fakeID := uuid.New().String()
	req := httptest.NewRequest(http.MethodGet, "/jobs/"+fakeID+"/trace", nil)
	rr := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want status %d for unknown job, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestJobLLMCalls(t *testing.T) {
	ms := newMemStore()
	h := NewHandler(ms)

	created, _ := ms.CreateJob(context.Background(), "cp_solve", json.RawMessage(`{}`), 0, "")
	ms.RecordLLMCall(context.Background(), store.LLMCall{
		JobID:            created.ID,
		Backend:          "groq",
		PromptTokens:     150,
		CompletionTokens: 50,
		EstimatedTokens:  250,
		LatencyMs:        320,
	})

	req := httptest.NewRequest(http.MethodGet, "/jobs/"+created.ID.String()+"/llm_calls", nil)
	rr := httptest.NewRecorder()
	newTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, got %d", http.StatusOK, rr.Code)
	}
	var calls []store.LLMCall
	if err := json.NewDecoder(rr.Body).Decode(&calls); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 llm call, got %d", len(calls))
	}
	if calls[0].PromptTokens != 150 || calls[0].CompletionTokens != 50 {
		t.Errorf("unexpected tokens in llm call: %+v", calls[0])
	}
}

func TestCreateJob_AdmissionControl_429(t *testing.T) {
	ms := newMemStore()
	// Limit = 2
	h := NewHandler(ms, 2)

	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create 2 jobs -> succeeds
	for i := 0; i < 2; i++ {
		body := `{"task_type":"cp_solve","payload":{"segment":1}}`
		req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(body))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 3rd job -> rejected with 429
	body := `{"task_type":"cp_solve","payload":{"segment":1}}`
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %d: %s", rec.Code, rec.Body.String())
	}
	if retryAfter := rec.Header().Get("Retry-After"); retryAfter != "5" {
		t.Errorf("expected Retry-After: 5, got %q", retryAfter)
	}

	var errResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal 429 response: %v", err)
	}
	if errResp["error"] != "queue at capacity" {
		t.Errorf("expected error 'queue at capacity', got %v", errResp["error"])
	}
	if pending, ok := errResp["pending"].(float64); !ok || int(pending) != 2 {
		t.Errorf("expected pending 2, got %v", errResp["pending"])
	}
}


