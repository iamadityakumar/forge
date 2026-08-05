package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is a per-process set of Prometheus metrics on its own registry.
// Construct with New(namespace) — do NOT use the global default registry,
// so tests are isolated and parallel-safe.
type Metrics struct {
	Registry *prometheus.Registry

	// --- API / orchestrator layer ---
	HTTPRequests        *prometheus.CounterVec   // {method, route, status}
	HTTPRequestDuration *prometheus.HistogramVec // {method, route}
	JobsSubmitted       *prometheus.CounterVec   // {task_type}
	JobsRejected        *prometheus.CounterVec   // {reason}
	PendingJobs         prometheus.Gauge
	ActiveWorkers       prometheus.Gauge

	// --- worker / agent layer ---
	ClaimsTotal      prometheus.Counter
	JobsCompleted    prometheus.Counter
	JobsFailed       *prometheus.CounterVec   // {dead_letter}
	JobDuration      prometheus.Histogram
	LeaseExtensions  prometheus.Counter
	InFlightJobs     prometheus.Gauge
	StepsTotal       *prometheus.CounterVec   // {step_type}
	StepDuration     *prometheus.HistogramVec // {step_type}
	StepsResumed     prometheus.Counter

	// --- LLM / rate-limit layer ---
	LLMCalls          *prometheus.CounterVec   // {backend}
	LLMDuration       *prometheus.HistogramVec // {backend}
	LLMTokens         *prometheus.CounterVec   // {backend, kind=prompt|completion}
	LLMErrors         *prometheus.CounterVec   // {backend, kind}  kind = bounded error category
	RateLimitWaits    *prometheus.CounterVec   // {limiter}
	RateLimitWaitTime *prometheus.HistogramVec // {limiter}
}

func New(namespace string) *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,
	}

	// Namespace prefix for all metric names (e.g., "forge" -> "forge_http_requests_total")
	ns := namespace
	if ns == "" {
		ns = "forge"
	}

	// --- API / orchestrator layer ---
	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "http_requests_total",
		Help:      "Total number of HTTP requests by method, route, and status.",
	}, []string{"method", "route", "status"})

	m.HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request latency in seconds by method and route.",
		Buckets:   LatencyBuckets,
	}, []string{"method", "route"})

	m.JobsSubmitted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "jobs_submitted_total",
		Help:      "Total number of jobs submitted by task type.",
	}, []string{"task_type"})

	m.JobsRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "jobs_rejected_total",
		Help:      "Total number of jobs rejected by reason (invalid, capacity).",
	}, []string{"reason"})

	m.PendingJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns,
		Name:      "pending_jobs",
		Help:      "Current number of pending jobs in the queue.",
	})

	m.ActiveWorkers = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns,
		Name:      "active_workers",
		Help:      "Current number of active workers (heartbeat within window).",
	})

	// --- worker / agent layer ---
	m.ClaimsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "claims_total",
		Help:      "Total number of job claims by workers.",
	})

	m.JobsCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "jobs_completed_total",
		Help:      "Total number of jobs completed successfully.",
	})

	m.JobsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "jobs_failed_total",
		Help:      "Total number of jobs failed by dead-letter status.",
	}, []string{"dead_letter"})

	m.JobDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns,
		Name:      "job_duration_seconds",
		Help:      "Job execution duration in seconds.",
		Buckets:   LatencyBuckets,
	})

	m.LeaseExtensions = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "lease_extensions_total",
		Help:      "Total number of successful lease extensions.",
	})

	m.InFlightJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns,
		Name:      "in_flight_jobs",
		Help:      "Current number of jobs being executed by this worker.",
	})

	m.StepsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "steps_total",
		Help:      "Total number of agent steps executed by step type.",
	}, []string{"step_type"})

	m.StepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Name:      "step_duration_seconds",
		Help:      "Agent step latency in seconds by step type.",
		Buckets:   StepBuckets,
	}, []string{"step_type"})

	m.StepsResumed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "steps_resumed_total",
		Help:      "Total number of steps resumed from checkpoint on worker handoff.",
	})

	// --- LLM / rate-limit layer ---
	m.LLMCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "llm_calls_total",
		Help:      "Total number of LLM API calls by backend.",
	}, []string{"backend"})

	m.LLMDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Name:      "llm_duration_seconds",
		Help:      "LLM API call latency in seconds by backend.",
		Buckets:   LatencyBuckets,
	}, []string{"backend"})

	m.LLMTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "llm_tokens_total",
		Help:      "Total LLM tokens consumed by backend and kind (prompt, completion).",
	}, []string{"backend", "kind"})

	m.LLMErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "llm_errors_total",
		Help:      "Total LLM errors by backend and error kind.",
	}, []string{"backend", "kind"})

	m.RateLimitWaits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Name:      "rate_limit_waits_total",
		Help:      "Total number of times LLM call waited for rate limit by limiter.",
	}, []string{"limiter"})

	m.RateLimitWaitTime = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Name:      "rate_limit_wait_seconds",
		Help:      "Time spent waiting for rate limit by limiter.",
		Buckets:   LatencyBuckets,
	}, []string{"limiter"})

	// Register all metrics with the custom registry
	reg.MustRegister(
		m.HTTPRequests,
		m.HTTPRequestDuration,
		m.JobsSubmitted,
		m.JobsRejected,
		m.PendingJobs,
		m.ActiveWorkers,
		m.ClaimsTotal,
		m.JobsCompleted,
		m.JobsFailed,
		m.JobDuration,
		m.LeaseExtensions,
		m.InFlightJobs,
		m.StepsTotal,
		m.StepDuration,
		m.StepsResumed,
		m.LLMCalls,
		m.LLMDuration,
		m.LLMTokens,
		m.LLMErrors,
		m.RateLimitWaits,
		m.RateLimitWaitTime,
	)

	return m
}

// Handler returns an http.Handler that exposes the metrics in Prometheus exposition format.
// Use this in HTTP servers to serve /metrics.
// Returns a handler that serves 404 if m is nil.
func (m *Metrics) Handler() http.Handler {
	if m == nil || m.Registry == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "metrics disabled", http.StatusNotFound)
		})
	}
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ServeHTTP implements http.Handler for backward compatibility.
// It delegates to Handler().
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.Handler().ServeHTTP(w, r)
}