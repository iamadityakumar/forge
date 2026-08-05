package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"forge/internal/metrics"
	"forge/internal/store"
)

// pingFailingStore wraps any JobStore and fails Ping, to exercise the
// degraded-but-200 readiness path.
type pingFailingStore struct {
	store.JobStore
}

func (s *pingFailingStore) Ping(ctx context.Context) error {
	return errors.New("db unreachable")
}

func TestMetricsMiddleware_RecordsRoutePatternAndStatus(t *testing.T) {
	m := metrics.New("test")
	h := NewHandler(newMemStore()).WithMetrics(m)

	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// GET /jobs
	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status 200, got %d", rr.Code)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()

	if !strings.Contains(out, `test_http_requests_total{method="GET",route="/jobs",status="200"}`) {
		t.Errorf("metrics render missing http_requests_total for GET /jobs:\n%s", out)
	}

	// 404 on unknown route
	req404 := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	rr404 := httptest.NewRecorder()
	r.ServeHTTP(rr404, req404)

	if rr404.Code != http.StatusNotFound {
		t.Fatalf("want status 404, got %d", rr404.Code)
	}

	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out = rec.Body.String()

	if !strings.Contains(out, `status="404"`) {
		t.Errorf("metrics render missing status=404:\n%s", out)
	}
}

func TestHealthEndpoint_EnrichedShape(t *testing.T) {
	h := NewHandler(newMemStore())

	r := chi.NewRouter()
	RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status 200, got %d", rr.Code)
	}

	var data map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&data); err != nil {
		t.Fatalf("decode health json: %v", err)
	}

	for _, key := range []string{"status", "db", "workers_online", "pending_jobs", "version", "uptime_seconds"} {
		if _, ok := data[key]; !ok {
			t.Errorf("health JSON missing required field %q in %+v", key, data)
		}
	}
}

func TestHealthEndpoint_DegradedStays200(t *testing.T) {
	h := NewHandler(&pingFailingStore{JobStore: newMemStore()})

	r := chi.NewRouter()
	RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// The probe stays up (200) even when degraded so a checker can read why.
	if rr.Code != http.StatusOK {
		t.Fatalf("degraded health must stay 200, got %d", rr.Code)
	}

	var data map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&data); err != nil {
		t.Fatalf("decode health json: %v", err)
	}
	if data["status"] != "degraded" {
		t.Errorf(`want status "degraded", got %v`, data["status"])
	}
	if !strings.Contains(fmt.Sprint(data["db"]), "db unreachable") {
		t.Errorf(`want db field to carry the failure reason, got %v`, data["db"])
	}
}

func TestWorkerMetricsProxy_SuccessAndOffline(t *testing.T) {
	// Mock worker metrics server
	workerTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("forge_claims_total 10\n"))
	}))
	defer workerTS.Close()

	urls := map[string]string{
		"worker-1": workerTS.URL,
		"worker-2": "http://127.0.0.1:59999/metrics", // dead port
	}

	h := NewHandler(newMemStore()).WithWorkerURLs(urls)

	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Proxy online worker
	req1 := httptest.NewRequest(http.MethodGet, "/api/worker-metrics/worker-1", nil)
	rr1 := httptest.NewRecorder()
	r.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("worker-1 proxy status = %d, want 200", rr1.Code)
	}
	if !strings.Contains(rr1.Body.String(), "forge_claims_total 10") {
		t.Fatalf("worker-1 proxy body missing metric:\n%s", rr1.Body.String())
	}

	// Proxy offline worker
	req2 := httptest.NewRequest(http.MethodGet, "/api/worker-metrics/worker-2", nil)
	rr2 := httptest.NewRecorder()
	r.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusBadGateway {
		t.Fatalf("worker-2 proxy status = %d, want 502 Bad Gateway", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "status=offline") {
		t.Fatalf("worker-2 proxy body missing offline marker:\n%s", rr2.Body.String())
	}
}