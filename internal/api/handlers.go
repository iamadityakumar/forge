package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Handler holds the dependencies needed by the API endpoints.
type Handler struct {
	store *Store
}

// NewHandler builds a Handler backed by the given Store.
func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

type createJobRequest struct {
	Task string `json:"task"`
}

// createJobHandler handles POST /jobs.
// It parses `{ "task": "..." }`, creates a job record, and returns
// `{ "job_id": "...", "status": "queued" }` with HTTP 201.
func (h *Handler) createJobHandler(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Task == "" {
		writeError(w, http.StatusBadRequest, "field 'task' is required")
		return
	}

	job := h.store.Create(req.Task)
	writeJSON(w, http.StatusCreated, job)
}

// getJobHandler handles GET /jobs/{id}.
// It returns the current job status, or 404 if the job does not exist.
func (h *Handler) getJobHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, ok := h.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
