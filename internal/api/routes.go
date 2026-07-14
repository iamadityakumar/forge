package api

import (
	"github.com/go-chi/chi/v5"
)

// RegisterRoutes wires the HTTP endpoints onto the provided chi router.
func RegisterRoutes(r chi.Router, h *Handler) {
	// POST /jobs will create a new job record.
	r.Post("/jobs", h.createJobHandler)

	// GET /jobs/{id} will retrieve a job status.
	r.Get("/jobs/{id}", h.getJobHandler)
}
