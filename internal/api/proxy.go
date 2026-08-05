package api

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// workerMetricsProxy returns an http.HandlerFunc that proxies GET requests for a worker's /metrics
// to the target URL configured in workerURLs[worker].
func workerMetricsProxy(workerURLs map[string]string) http.HandlerFunc {
	client := &http.Client{Timeout: 10 * time.Second}

	return func(w http.ResponseWriter, r *http.Request) {
		workerName := chi.URLParam(r, "worker")
		targetURL, ok := workerURLs[workerName]
		if !ok || targetURL == "" {
			writeError(w, http.StatusNotFound, fmt.Sprintf("worker %q metrics URL not configured", workerName))
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create proxy request")
			return
		}

		resp, err := client.Do(req)
		if err != nil {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "worker=%s status=offline error=%v\n", workerName, err)
			return
		}
		defer resp.Body.Close()

		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
