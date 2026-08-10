package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"forge/internal/metrics"
	"forge/internal/store"
)

// Handler holds the dependencies needed by the API endpoints.
type Handler struct {
	store          store.JobStore
	maxPendingJobs int
	metrics        *metrics.Metrics
	workerURLs     map[string]string
	startTime      time.Time
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
		workerURLs:     make(map[string]string),
		startTime:      time.Now(),
	}
}

// WithMetrics attaches a metrics store to the Handler.
func (h *Handler) WithMetrics(m *metrics.Metrics) *Handler {
	h.metrics = m
	return h
}

// WithWorkerURLs attaches a map of worker name -> metrics URL to the Handler.
func (h *Handler) WithWorkerURLs(urls map[string]string) *Handler {
	if urls != nil {
		h.workerURLs = urls
	}
	return h
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
		if h.metrics != nil {
			h.metrics.JobsRejected.WithLabelValues("invalid").Inc()
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.TaskType == "" {
		if h.metrics != nil {
			h.metrics.JobsRejected.WithLabelValues("invalid").Inc()
		}
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
			if h.metrics != nil {
				h.metrics.JobsRejected.WithLabelValues("capacity").Inc()
			}
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

	if h.metrics != nil {
		h.metrics.JobsSubmitted.WithLabelValues(req.TaskType).Inc()
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
	opts := store.ListJobsOpts{
		Status:   r.URL.Query().Get("status"),
		TaskType: r.URL.Query().Get("task_type"),
		WorkerID: r.URL.Query().Get("worker"),
		Limit:    50,
		Offset:   0,
	}

	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if since, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			opts.Since = &since
		}
	}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 {
			opts.Limit = limit
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if offset, err := strconv.Atoi(offsetStr); err == nil && offset >= 0 {
			opts.Offset = offset
		}
	}

	jobs, err := h.store.ListJobs(r.Context(), opts)
	if err != nil {
		slog.Error("list jobs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}

	if h.metrics != nil {
		if pending, err := h.store.CountPendingJobs(r.Context()); err == nil {
			h.metrics.PendingJobs.Set(float64(pending))
		}
		if workers, err := h.store.CountActiveWorkers(r.Context(), 30*time.Second); err == nil {
			h.metrics.ActiveWorkers.Set(float64(workers))
		}
	}

	writeJSON(w, http.StatusOK, jobs)
}

// ---------------------------------------------------------------------------
// GET /jobs/{id}/trace — U4 step timeline
// ---------------------------------------------------------------------------

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
		steps = []store.JobStep{}
	}
	writeJSON(w, http.StatusOK, steps)
}

// ---------------------------------------------------------------------------
// GET /jobs/{id}/llm_calls
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// GET /api/workers — List configured worker names for dashboard discovery
// ---------------------------------------------------------------------------

func (h *Handler) listWorkersHandler(w http.ResponseWriter, r *http.Request) {
	workers := make([]string, 0, len(h.workerURLs))
	for k := range h.workerURLs {
		workers = append(workers, k)
	}
	sort.Strings(workers)
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

// ---------------------------------------------------------------------------
// GET /api/workers/metrics — Batched fetch of all worker metrics
// ---------------------------------------------------------------------------

func (h *Handler) listWorkersMetricsHandler(w http.ResponseWriter, r *http.Request) {
	type workerMetrics struct {
		Name    string `json:"name"`
		Online  bool   `json:"online"`
		Metrics string `json:"metrics"` // raw Prometheus text
		Error   string `json:"error,omitempty"`
	}

	var wg sync.WaitGroup
	results := make([]workerMetrics, len(h.workerURLs))
	i := 0
	for name := range h.workerURLs {
		results[i].Name = name
		i++
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	for idx := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := results[idx].Name
			targetURL := h.workerURLs[name]

			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
			if err != nil {
				results[idx].Error = err.Error()
				return
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results[idx].Online = false
				results[idx].Error = err.Error()
				return
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				results[idx].Error = err.Error()
				return
			}

			results[idx].Online = resp.StatusCode == http.StatusOK
			results[idx].Metrics = string(body)
			if resp.StatusCode != http.StatusOK {
				results[idx].Error = fmt.Sprintf("status %d", resp.StatusCode)
			}
		}(idx)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{"workers": results})
}

// ---------------------------------------------------------------------------
// GET /health — Enriched readiness & status probe
// ---------------------------------------------------------------------------

func (h *Handler) healthHandler(w http.ResponseWriter, r *http.Request) {
	dbStatus := "ok"
	if err := h.store.Ping(r.Context()); err != nil {
		dbStatus = fmt.Sprintf("error: %v", err)
	}

	pendingJobs, _ := h.store.CountPendingJobs(r.Context())
	workersOnline, _ := h.store.CountActiveWorkers(r.Context(), 30*time.Second)

	overallStatus := "ok"
	if dbStatus != "ok" || workersOnline == 0 {
		overallStatus = "degraded"
	}

	uptimeSeconds := int(time.Since(h.startTime).Seconds())

	resp := map[string]any{
		"status":         overallStatus,
		"db":             dbStatus,
		"workers_online": workersOnline,
		"pending_jobs":   pendingJobs,
		"version":        "0.6.0",
		"uptime_seconds": uptimeSeconds,
	}

	// The probe stays up (200) even when degraded so an external checker can
	// read *why* from the body — status:"degraded" carries the signal, not the
	// HTTP code.
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// statsHandler reports job counts aggregated from the jobs table (source of
// truth) rather than per-process in-memory counters, so dashboard tiles stay
// consistent across restarts.
func (h *Handler) statsHandler(w http.ResponseWriter, r *http.Request) {
	counts, err := h.store.CountJobs(r.Context())
	if err != nil {
		slog.Error("count jobs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to count jobs")
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
