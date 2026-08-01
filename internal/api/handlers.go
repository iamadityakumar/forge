package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"forge/internal/store"
)

// Handler holds the dependencies needed by the API endpoints.
type Handler struct {
	store          store.JobStore
	maxPendingJobs int
}

// NewHandler builds a Handler backed by the given JobStore, with optional
// admission control cap maxPendingJobs (0 = unlimited).
func NewHandler(s store.JobStore, maxPendingJobs ...int) *Handler {
	limit := 0
	if len(maxPendingJobs) > 0 {
		limit = maxPendingJobs[0]
	}
	return &Handler{
		store:          s,
		maxPendingJobs: limit,
	}
}

// ---------------------------------------------------------------------------
// POST /jobs
// ---------------------------------------------------------------------------

type createJobRequest struct {
	TaskType       string          `json:"task_type"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int             `json:"priority"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (h *Handler) createJobHandler(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.TaskType == "" {
		writeError(w, http.StatusBadRequest, "field 'task_type' is required")
		return
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}

	if h.maxPendingJobs > 0 {
		pending, err := h.store.CountPendingJobs(r.Context())
		if err != nil {
			slog.Error("count pending jobs failed", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to check queue capacity")
			return
		}
		if pending >= h.maxPendingJobs {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":   "queue at capacity",
				"pending": pending,
			})
			return
		}
	}

	job, err := h.store.CreateJob(r.Context(), req.TaskType, req.Payload, req.Priority, req.IdempotencyKey)
	if err != nil {
		slog.Error("create job failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// ---------------------------------------------------------------------------
// GET /jobs/{id}
// ---------------------------------------------------------------------------

func (h *Handler) getJobHandler(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job ID format")
		return
	}

	job, err := h.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		slog.Error("get job failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// ---------------------------------------------------------------------------
// GET /jobs
// ---------------------------------------------------------------------------

func (h *Handler) listJobsHandler(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 50

	jobs, err := h.store.ListJobs(r.Context(), status, limit)
	if err != nil {
		slog.Error("list jobs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	if jobs == nil {
		jobs = []store.Job{} // never return null JSON
	}
	writeJSON(w, http.StatusOK, jobs)
}

// ---------------------------------------------------------------------------
// GET /jobs/{id}/trace — the demo's money shot (U4)
// ---------------------------------------------------------------------------

// jobTraceHandler returns the ordered checkpointed steps of a job, so the
// dashboard can render the live step timeline as a recovering job fills in
// (segments 1..k appearing one at a time after a kill -> resume). A nonexistent
// job yields 404 (validated via GetJob); a job with no steps yet yields [].
func (h *Handler) jobTraceHandler(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job ID format")
		return
	}

	if _, err := h.store.GetJob(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	} else if err != nil {
		slog.Error("get job for trace failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}

	steps, err := h.store.ListSteps(r.Context(), id)
	if err != nil {
		slog.Error("list steps failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list steps")
		return
	}
	if steps == nil {
		steps = []store.JobStep{} // never return null JSON
	}
	writeJSON(w, http.StatusOK, steps)
}

// ---------------------------------------------------------------------------
// GET /health
// ---------------------------------------------------------------------------

func (h *Handler) healthHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}// ---------------------------------------------------------------------------
// GET /jobs/{id}/llm_calls
// ---------------------------------------------------------------------------

// jobLLMCallsHandler returns all recorded LLM calls for a job.
func (h *Handler) jobLLMCallsHandler(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job ID format")
		return
	}

	if _, err := h.store.GetJob(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	} else if err != nil {
		slog.Error("get job for llm_calls failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}

	calls, err := h.store.ListLLMCalls(r.Context(), id)
	if err != nil {
		slog.Error("list llm calls failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list llm calls")
		return
	}
	if calls == nil {
		calls = []store.LLMCall{}
	}
	writeJSON(w, http.StatusOK, calls)
}
