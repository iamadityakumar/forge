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
	store store.JobStore
}

// NewHandler builds a Handler backed by the given JobStore.
func NewHandler(s store.JobStore) *Handler {
	return &Handler{store: s}
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
}
