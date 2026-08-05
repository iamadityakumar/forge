package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"forge/internal/metrics"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// MetricsMiddleware wraps an http.Handler to measure HTTP request count and latency
// using bounded route patterns from Chi context (e.g. "/jobs/{id}").
func MetricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m == nil {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			rctx := chi.RouteContext(r.Context())
			route := ""
			if rctx != nil {
				route = rctx.RoutePattern()
			}
			if route == "" {
				route = "unknown"
			}

			dur := time.Since(start)
			m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).Inc()
			m.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())
		})
	}
}