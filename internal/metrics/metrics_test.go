package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetrics_CounterIncrements(t *testing.T) {
	m := New("test")

	m.ClaimsTotal.Inc()
	m.ClaimsTotal.Inc()

	m.LLMCalls.WithLabelValues("groq").Inc()
	m.LLMCalls.WithLabelValues("groq").Inc()

	m.LLMTokens.WithLabelValues("groq", "prompt").Add(42)

	// Verify via handler
	handler := m.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		"test_claims_total 2",
		`test_llm_calls_total{backend="groq"} 2`,
		`test_llm_tokens_total{backend="groq",kind="prompt"} 42`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in output:\n%s", want, body)
		}
	}
}

func TestMetrics_Gauge(t *testing.T) {
	m := New("test")

	m.InFlightJobs.Set(5)
	handler := m.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "test_in_flight_jobs 5") {
		t.Errorf("in_flight_jobs = 0, want 5; body: %s", rec.Body.String())
	}

	m.InFlightJobs.Inc()
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "test_in_flight_jobs 6") {
		t.Errorf("in_flight_jobs after inc = 0, want 6; body: %s", rec.Body.String())
	}

	m.InFlightJobs.Dec()
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "test_in_flight_jobs 5") {
		t.Errorf("in_flight_jobs after dec = 0, want 5; body: %s", rec.Body.String())
	}

	m.InFlightJobs.Add(-2)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "test_in_flight_jobs 3") {
		t.Errorf("in_flight_jobs after add = 0, want 3; body: %s", rec.Body.String())
	}
}

func TestMetrics_Histogram(t *testing.T) {
	m := New("test")

	m.StepDuration.WithLabelValues("plan").Observe(0.05)
	m.StepDuration.WithLabelValues("plan").Observe(0.3)
	m.StepDuration.WithLabelValues("plan").Observe(1.5)

	handler := m.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	out := rec.Body.String()

	// StepBuckets = [.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120]
	expected := []string{
		`test_step_duration_seconds_bucket{step_type="plan",le="0.05"} 1`,
		`test_step_duration_seconds_bucket{step_type="plan",le="0.1"} 1`,
		`test_step_duration_seconds_bucket{step_type="plan",le="0.25"} 1`,
		`test_step_duration_seconds_bucket{step_type="plan",le="0.5"} 2`,
		`test_step_duration_seconds_bucket{step_type="plan",le="1"} 2`,
		`test_step_duration_seconds_bucket{step_type="plan",le="2.5"} 3`,
		`test_step_duration_seconds_bucket{step_type="plan",le="5"} 3`,
		`test_step_duration_seconds_bucket{step_type="plan",le="10"} 3`,
		`test_step_duration_seconds_bucket{step_type="plan",le="30"} 3`,
		`test_step_duration_seconds_bucket{step_type="plan",le="60"} 3`,
		`test_step_duration_seconds_bucket{step_type="plan",le="120"} 3`,
		`test_step_duration_seconds_bucket{step_type="plan",le="+Inf"} 3`,
		`test_step_duration_seconds_sum{step_type="plan"} 1.85`,
		`test_step_duration_seconds_count{step_type="plan"} 3`,
	}

	for _, want := range expected {
		if !strings.Contains(out, want) {
			t.Errorf("histogram render missing %q in:\n%s", want, out)
		}
	}
}

func TestMetrics_NoGlobalRegistry(t *testing.T) {
	m1 := New("instance_1")
	m2 := New("instance_2")

	m1.ClaimsTotal.Inc()

	handler1 := m1.Handler()
	rec1 := httptest.NewRecorder()
	handler1.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	handler2 := m2.Handler()
	rec2 := httptest.NewRecorder()
	handler2.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if !strings.Contains(rec1.Body.String(), "instance_1_claims_total 1") {
		t.Fatalf("m1 claims not found: %s", rec1.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "instance_2_claims_total 1") {
		t.Fatalf("m2 claims should be 0 (isolated): %s", rec2.Body.String())
	}
}

func TestMetrics_NilSafe(t *testing.T) {
	// Test that ServeHTTP on nil metrics returns 404
	var m *Metrics
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil metrics ServeHTTP status = %d, want 404", rec.Code)
	}
}

func TestMetrics_Handler(t *testing.T) {
	m := New("test")
	m.ClaimsTotal.Inc()
	m.JobsCompleted.Inc()
	m.InFlightJobs.Set(3)
	m.StepDuration.WithLabelValues("plan").Observe(0.1)

	handler := m.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE test_claims_total counter",
		"test_claims_total 1",
		"# TYPE test_jobs_completed_total counter",
		"test_jobs_completed_total 1",
		"# TYPE test_in_flight_jobs gauge",
		"test_in_flight_jobs 3",
		"# TYPE test_step_duration_seconds histogram",
		"test_step_duration_seconds_bucket",
		"test_step_duration_seconds_sum",
		"test_step_duration_seconds_count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("handler output missing %q", want)
		}
	}

	// Verify content type (promhttp adds escaping=underscores)
	ct := rec.Header().Get("Content-Type")
	if ct != "text/plain; version=0.0.4; charset=utf-8; escaping=underscores" {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4; charset=utf-8; escaping=underscores", ct)
	}
}

func TestMetrics_LabelCardinality(t *testing.T) {
	m := New("test")

	// Simulate bounded label sets from real usage
	for _, backend := range []string{"groq", "ollama", "fake"} {
		m.LLMCalls.WithLabelValues(backend).Inc()
		m.LLMDuration.WithLabelValues(backend).Observe(0.1)
		for _, kind := range []string{"prompt", "completion"} {
			m.LLMTokens.WithLabelValues(backend, kind).Add(100)
		}
		for _, kind := range []string{"timeout", "rate_limit", "auth", "provider", "network", "internal", "canceled"} {
			m.LLMErrors.WithLabelValues(backend, kind).Inc()
		}
	}

	for _, stepType := range []string{"plan", "tool_call"} {
		m.StepsTotal.WithLabelValues(stepType).Inc()
		m.StepDuration.WithLabelValues(stepType).Observe(0.1)
	}

	for _, limiter := range []string{"memory", "upstash"} {
		m.RateLimitWaits.WithLabelValues(limiter).Inc()
		m.RateLimitWaitTime.WithLabelValues(limiter).Observe(0.1)
	}

	for _, method := range []string{"GET", "POST"} {
		for _, route := range []string{"/jobs", "/jobs/{id}", "/health", "/metrics"} {
			for _, status := range []string{"200", "201", "400", "404", "429", "500"} {
				m.HTTPRequests.WithLabelValues(method, route, status).Inc()
				m.HTTPRequestDuration.WithLabelValues(method, route).Observe(0.01)
			}
		}
	}

	// Collect all metrics and verify no unexpected label combinations
	gatherer := prometheus.Gatherers{m.Registry}
	metricFamilies, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gather failed: %v", err)
	}

	for _, mf := range metricFamilies {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "job_id" || lp.GetName() == "uuid" {
					t.Errorf("found unbounded label %s on metric %s", lp.GetName(), mf.GetName())
				}
			}
		}
	}
}
