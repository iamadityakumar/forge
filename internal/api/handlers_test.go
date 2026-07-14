package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func newTestRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func TestCreateJob(t *testing.T) {
	h := NewHandler(NewStore())
	ts := httptest.NewServer(newTestRouter(h))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/jobs", "application/json", strings.NewReader(`{"task":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want status %d, got %d", http.StatusCreated, resp.StatusCode)
	}

	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected non-empty job_id")
	}
	if job.Status != StatusQueued {
		t.Fatalf("want status %q, got %q", StatusQueued, job.Status)
	}
}

func TestCreateJobMissingTask(t *testing.T) {
	h := NewHandler(NewStore())
	ts := httptest.NewServer(newTestRouter(h))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/jobs", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want status %d, got %d", http.StatusBadRequest, resp.StatusCode)
	}
}

func TestGetJob(t *testing.T) {
	h := NewHandler(NewStore())
	created := h.store.Create("ping")

	req := httptest.NewRequest(http.MethodGet, "/jobs/"+created.ID, nil)
	rr := httptest.NewRecorder()

	r := chi.NewRouter()
	RegisterRoutes(r, h)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, got %d", http.StatusOK, rr.Code)
	}

	var job Job
	if err := json.NewDecoder(rr.Body).Decode(&job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID != created.ID {
		t.Fatalf("want id %q, got %q", created.ID, job.ID)
	}
}

func TestGetJobNotFound(t *testing.T) {
	h := NewHandler(NewStore())

	req := httptest.NewRequest(http.MethodGet, "/jobs/missing", nil)
	rr := httptest.NewRecorder()

	r := chi.NewRouter()
	RegisterRoutes(r, h)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want status %d, got %d", http.StatusNotFound, rr.Code)
	}
}
