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
	jobs   map[uuid.UUID]store.Job
	steps  map[uuid.UUID][]store.JobStep
}

func newMemStore() *memStore {
	return &memStore{
		jobs:  make(map[uuid.UUID]store.Job),
		steps: make(map[uuid.UUID][]store.JobStep),
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

func (m *memStore) GetJob(_ context.Context, id uuid.UUID) (store.Job, error) {
	j, ok := m.jobs[id]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return j, nil
}

func (m *memStore) ListJobs(_ context.Context, status string, limit int) ([]store.Job, error) {
	var out []store.Job
	for _, j := range m.jobs {
		// status=="dead_letter" is a virtual filter (no real job carries it);
		// the in-memory mock has no dead-letter jobs, so it returns [].
		if status == "" || status == store.StatusDeadLetter || j.Status == status {
			out = append(out, j)
		}
		if len(out) >= limit {
			break
		}
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
