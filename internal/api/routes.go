package api

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
)

// RegisterRoutes wires the HTTP endpoints onto the provided chi router.
func RegisterRoutes(r chi.Router, h *Handler) {
	if h.metrics != nil {
		r.Use(MetricsMiddleware(h.metrics))
		r.Handle("/metrics", h.metrics)
	}

	r.Get("/health", h.healthHandler)

	r.Post("/jobs", h.createJobHandler)
	r.Get("/jobs", h.listJobsHandler)
	r.Get("/jobs/{id}", h.getJobHandler)
	r.Get("/jobs/{id}/trace", h.jobTraceHandler)
	r.Get("/jobs/{id}/llm_calls", h.jobLLMCallsHandler)

	r.Get("/api/workers", h.listWorkersHandler)
	r.Get("/api/workers/metrics", h.listWorkersMetricsHandler)
	r.Get("/api/worker-metrics/{worker}", workerMetricsProxy(h.workerURLs))

	// Dashboard static files serving
	workDir, _ := os.Getwd()
	webDir := filepath.Join(workDir, "web")
	if _, err := os.Stat(webDir); err == nil {
		fileServer := http.FileServer(http.Dir(webDir))
		r.Get("/dashboard", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
		})
		r.Handle("/static/*", http.StripPrefix("/static/", fileServer))
	}
}
