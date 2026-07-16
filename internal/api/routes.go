package api

import (
	"github.com/go-chi/chi/v5"
)

// RegisterRoutes wires the HTTP endpoints onto the provided chi router.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Get("/health", h.healthHandler)

	r.Post("/jobs", h.createJobHandler)
	r.Get("/jobs", h.listJobsHandler)
	r.Get("/jobs/{id}", h.getJobHandler)
}
