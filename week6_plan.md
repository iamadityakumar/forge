# Week 6 Plan — Observability: structured logging, Prometheus metrics, and a live dashboard

> Thesis: Weeks 1–5 made Forge **correct** (atomic claiming, fencing, crash
> recovery) and **safe** (cost-aware rate limiting). Week 6 makes it
> **observable** — you can *see* it work, *explain* why it's slow, and *answer*
> "is this system unhealthy in production?" from a single link-shareable
> dashboard. Structured `slog` logs everywhere, Prometheus-format metrics for
> every layer (API, worker, agent, LLM, rate limiter), and a plain-HTML/JS
> dashboard that polls the real APIs and renders live — **no Prometheus server,
> no Grafana, no new paid infrastructure**. Stretch: OpenTelemetry distributed
> tracing (U8) that makes a `kill -9` mid-job render as *one trace* continuing
> across workers.

The textbook observability move is "add a dashboard." The senior move is
*boundary instrumentation with bounded cardinality*: measure each layer where
its real cost lives, label by bounded dimensions (`route`, `task_type`,
`step_type`, `backend`), and never let a metric explode with unbounded labels
(raw job IDs, raw error strings). Week 6 does the textbook dashboard *and* the
defensible design choices that make the metrics trustworthy. The money shot is
the stretch: a cross-worker trace that shows a job flowing
`claim@worker-1 → steps → kill → reclaim@worker-2 → resume → complete` under
one trace ID — the Week 3 crash-recovery story made *visual*.

---

## Section 0 — Where we are now

### Completed so far

| Week | Thesis | Evidence |
|---|---|---|
| 1 | Job API + durable Postgres queue | `POST /jobs`, `GET /jobs`, chi router, `000001_initial` migration |
| 2 | Atomic claiming + fencing | `SELECT … FOR UPDATE SKIP LOCKED`, `lease_epoch` fence token, `000003_fencing_checkpoints` |
| 3 | Crash recovery | Self-renewing lease (`executeWithLease`/`extenderLoop`), reclaim of expired `running` jobs, `000004_step_worker_attribution`, invariant chaos test (`chaos_test.go`) |
| 4 | LLM agent + durable checkpoint loop | `LLMBackend` abstraction, `cp_solve` agent with `search_kb`/`run_tests`, `plan`/`tool_call` step protocol, **live HTTPS crash-recovery demo on 2026-07-31** (`scripts/cp_solve_agent_demo.sh`, two-worker trace, exactly-once steps) |
| 5 | Cost-aware rate limiting at the LLM boundary | In flight — `internal/ratelimit/`, `RateLimitedBackend` decorator, `MAX_PENDING_JOBS` admission control, `scripts/burst_load_test.sh` (worktree `week5-ratelimit`, unmerged on `main`) |

Week 6 sits **on top of** Weeks 4–5, not beside them. It instruments the exact
chokepoints those weeks built — the agent's step loop (`internal/agent/agent.go`),
the worker lifecycle (`internal/worker/worker.go`), the API router
(`internal/api/router.go`), and the LLM/rate-limit boundary
(`internal/llm/ratelimited.go`) — with zero restructuring. Every span point and
metric hook is a wrap-around, not a rewrite.

### Inherited scaffold (usable as-is)

- **`slog` throughout** — the worker, agent, API, LLM, and rate-limit paths
  already log via `log/slog`. There is **no centralized handler or env-driven
  level**: every process calls `slog.New(slog.NewTextHandler(os.Stderr, nil))`
  (or the default) and logs at `Info`. Phase 0 makes that configurable with
  `LOG_FORMAT`/`LOG_LEVEL` — the call sites are already there.
- **`RateLimitedBackend` decorator** (Week 5) — the LLM/rate-limit chokepoint.
  Every LLM call and every backpressure wait passes through one function;
  instrumenting it instruments *all* LLM traffic. Its `slog.Warn` backpressure
  line is the U8 span point.
- **`Usage{PromptTokens, CompletionTokens}`** (Week 4) — the data behind
  `forge_llm_tokens_total{kind=prompt|completion}`.
- **`FakeBackend` with `CallCount`** (Week 4) — deterministic, network-free
  metric/span assertions without a live LLM.
- **Per-step `worker_id` attribution** (migration `000004`) — `job_steps`
  records *which worker* wrote each step. This is what makes a reclaimed job's
  steps attributable to two different workers, which is what makes the U8
  cross-worker trace *renderable*.
- **`Clock`/`ManualClock`** (Week 5, `internal/ratelimit/clock.go`) — the U10
  seed; deterministic timing for histogram tests.
- **Week 5's counter-only Prometheus seed** (`internal/metrics/metrics.go` in
  the `week5-ratelimit` worktree) — four counters
  (`forge_llm_calls_total`, `forge_llm_tokens_total`,
  `forge_rate_limit_waits_total`, `forge_rate_limit_wait_seconds`) behind an
  optional `metrics` param, plus `cmd/worker/main.go` serving `/metrics` on
  `METRICS_PORT` (default **9091**). **This is the migration base for
  Phase 1**, not the final form (see the tension below).

### Absent (greenfield for Week 6)

- **`internal/log/`** does not exist — no centralized logging config.
- **`web/` does not exist anywhere** — the dashboard is 100% greenfield.
- **No `prometheus/client_golang`** dependency — `go.mod` has only chi, uuid,
  pgx, `golang.org/x/sync` (+ indirect). The Week 5 seed is hand-rolled and
  counter-only.
- **No `internal/metrics/` on `main`** — the Week 5 seed lives only in the
  unmerged worktree.
- **No OpenTelemetry** dependency, no `000005` migration.
- **`/health` is a stub** — no DB check, no worker count, no version/uptime.
- **No per-step latency histogram anywhere** — the implementation plan's
  required metric doesn't exist in the counter-only seed.

---

## Time & scope

- **Budget:** ~10–12 focused hours.
- **Core (must):** Phase 0 (structured logging config), Phase 1 (metrics
  package on `client_golang`), Phase 2 (instrumentation across API/worker/
  agent/LLM), Phase 3 (`/metrics` + enriched `/health`), Phase 4 (dashboard),
  Phase 5 (tests), Phase 7 (deploy + live demo).
- **Stretch (if time):** Phase 6 (U8 OpenTelemetry distributed tracing) and
  Phase 6b (optional Prometheus + Jaeger docker-compose observability profile).
- **Order dependency:** Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5 is a
  hard chain (each phase's tests build on the previous's metrics). Phase 0
  (logging) is **independent** and can land first or in parallel. Phase 6 (U8)
  depends on Phase 1's registry and Phase 2's span points; it is designed to be
  dropped without touching any core phase.

---

## Phase 0 — Structured logging, centralized (`internal/log/`)

### Goal

One place that decides **format** (`text` for humans, `json` for a log
aggregator) and **level** (`debug|info|warn|error`) for every process — driven
by env vars, with a zero-config default that matches today's behavior exactly.
No call-site changes; `slog.Default()` is set once at `main` and everything
else keeps calling `slog.X(...)` as it does today.

### Files

| File | Change |
|---|---|
| `internal/log/config.go` (new) | `Setup(service string) (*slog.Logger, error)` — parse `LOG_FORMAT`/`LOG_LEVEL`, build the handler, set `slog.SetDefault`, return a service-tagged logger |
| `cmd/orchestrator/main.go` | Call `log.Setup("orchestrator")` first; `defer` `logger` flush if any |
| `cmd/worker/main.go` | Call `log.Setup("worker-" + id)` first |
| `.env.example` | Document `LOG_FORMAT`/`LOG_LEVEL` |

### Behavior

```go
func Setup(service string) (*slog.Logger, error) {
    level, err := parseLevel(os.Getenv("LOG_LEVEL"))        // default slog.LevelInfo
    var h slog.Handler
    switch strings.ToLower(os.Getenv("LOG_FORMAT")) {       // default "text"
    case "json":
        h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
    default:
        h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
    }
    logger := slog.New(h).With("service", service)
    slog.SetDefault(logger)                                 // everyone keeps using slog.X
    return logger, nil
}
```

- `LOG_FORMAT` ∈ `text|json`, default `text` — today's output, unchanged.
- `LOG_LEVEL` ∈ `debug|info|warn|error`, default `info`.
- With `LOG_FORMAT=json LOG_LEVEL=debug`, every existing `slog.Warn(...)`,
  `slog.Info(...)` line becomes a parseable JSON object with a `service` key —
  verifiable with `jq`, and ready to feed any aggregator.
- Every `main` gets a one-line `logger` for process-level events ("worker
  starting", "orchestrator listening") that carry `service`.

### Interview angle

Why centralize in `internal/log` instead of scattering `slog.SetDefault`
calls? One, the *format/level decision is a deployment concern* — it must be
flippable by env in production without a rebuild. Two, `slog.Default()`
already routes everything (the codebase already logs via `slog`), so one
`Setup` at `main` makes the whole process honor `LOG_FORMAT`/`LOG_LEVEL` with
zero call-site churn. "I can turn debug on by redeploying with one env var,
and ship JSON logs to any collector" is the whole pitch.

### Acceptance

- `LOG_FORMAT=json LOG_LEVEL=debug` on either binary → each log line parses as
  JSON (`jq -e .` passes) and includes `service`, `time`, `level`, `msg`.
- `LOG_LEVEL=warn` → Info/Debug lines suppressed; Warn/Error still emitted.
- Default (no env) → byte-compatible with Week 5's text output.

---

## Phase 1 — Metrics package on `client_golang` (`internal/metrics/`)

### The decision: adopt `prometheus/client_golang`, migrate the Week 5 seed

The Week 5 seed is deliberately **counter-only and dependency-free**. The
implementation plan's Week 6 requires a **per-step latency histogram**, which
a hand-rolled counter store cannot express. Two options:

1. **Extend the seed** to hand-roll histograms (exponential buckets,
   cumulative sums, `_bucket`/`_sum`/`_count` lines, `+Inf` bucket).
2. **Adopt `prometheus/client_golang`** and re-point the four seed counters
   onto its typed metrics.

**Decision: option 2.** `client_golang` is the ecosystem standard the
implementation plan's tech-stack table already names; hand-rolling histogram
aggregation (thread-safe bucket update, exposition-format `_bucket`/`_sum`/
`_count` rendering) is a correctness trap with zero interview value — it's
reimplementing a maintained library. The Week 5 seed proved the *wiring*; this
phase replaces the *engine* it was a stand-in for. The four seed counters keep
their exact names and labels so any Week 5 dashboards/scripts keep working.

The cost is a `go get github.com/prometheus/client_golang` — a single,
standard, well-understood dependency, entirely in keeping with the project's
"standard library first, minimal deps" stance (we already take chi, pgx,
x/sync).

### Files

| File | Change |
|---|---|
| `go.mod` | `go get github.com/prometheus/client_golang@latest` |
| `internal/metrics/metrics.go` (rework) | `Metrics` struct holding typed Prometheus metrics + a per-process `*prometheus.Registry` |
| `internal/metrics/metrics_test.go` | Render tests: counters/histograms emit correct exposition; label cardinality is bounded |
| `internal/llm/ratelimited.go` | Re-point the four seed counters onto the new registry (signature: make `m *metrics.Metrics` a required param) |

### Behavior

Each process constructs its **own custom registry** — never the global default
`prometheus.DefaultRegisterer`. Custom registries are isolated, so tests can
spin up a `Metrics` and assert on it without polluting other tests or the
`-race` run. The `Metrics` struct groups every metric the process owns:

```go
package metrics

// Metrics is a per-process set of Prometheus metrics on its own registry.
// Construct with New(namespace) — do NOT use the global default registry,
// so tests are isolated and parallel-safe.
type Metrics struct {
    // registry is the process-local registry; Handler() serves it.
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
    StepDuration     *prometheus.HistogramVec // {step_type}   ← the required per-step latency histogram
    StepsResumed     prometheus.Counter

    // --- LLM / rate-limit layer ---
    LLMCalls          *prometheus.CounterVec   // {backend}
    LLMDuration       *prometheus.HistogramVec // {backend}
    LLMTokens         *prometheus.CounterVec   // {backend, kind=prompt|completion}
    LLMErrors         *prometheus.CounterVec   // {backend, kind}  kind = bounded error category
    RateLimitWaits    *prometheus.CounterVec   // {limiter}
    RateLimitWaitTime *prometheus.HistogramVec // {limiter}
}

func New(namespace string) *Metrics
func (m *Metrics) Handler() http.Handler // promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
```

**Histogram buckets** are shared constants in `internal/metrics/buckets.go`
and reused everywhere:

```go
var LatencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
var StepBuckets    = []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
```

### The cardinality rules (non-negotiable, this is the interview material)

- **Route patterns, not raw paths.** The middleware labels with
  `chi.RouteContext(r.Context()).RoutePattern()` → `/jobs/{id}`, **never** the
  concrete UUID. An unbounded `{job_id}` label would create one time-series per
  job ever — the #1 metric anti-pattern.
- **Worker identity is an *instance* label, not a metric label.** In a real
  Prometheus deployment, the scrape config's `instance` label carries it for
  free. With no Prometheus server, the dashboard fetches each worker's
  `/metrics` endpoint directly (Phase 3) and *knows* which worker it scraped.
  Putting `worker_id` in the metric labels would (a) explode series count as
  workers churn and (b) duplicate what the scrape target already encodes.
- **Bounded error categories, not raw error strings.** `classify(err)` maps any
  error to one of `timeout|rate_limit|auth|provider|network|internal`.
  Raw error strings as labels are unbounded and unstable across versions.

These three rules are what separate "I exposed metrics" from "metrics you can
actually alert on."

### Interview angle

The histogram requirement is the tell: a counter-only store can't express
latency percentiles, so the "I'll hand-roll it to stay dependency-free"
instinct collides with reality the moment you need `p95 step latency`. Using
`client_golang` means "the metric semantics are maintained by the ecosystem
and the *interesting* engineering is deciding *what* to measure and *how to
label it* — not re-deriving cumulative bucket math." The cardinality rules
above are the part an interviewer who's run Prometheus will probe.

### Acceptance

- `go get github.com/prometheus/client_golang` lands cleanly; `go mod tidy`
  shows no surprise transitive changes beyond the prometheus/common stack.
- `internal/metrics` tests pass under `-race` using a fresh `New("test")`
  per test — zero shared global state.
- Rendering `m.Handler()` produces valid exposition: `# HELP`/`# TYPE`,
  `_total` counters, `_bucket`/`_sum`/`_count` for histograms.
- The four Week 5 seed counters exist with identical names/labels on the new
  registry.

---

## Phase 2 — Instrumentation across the four layers

### Goal

Wire metrics into the exact chokepoints Weeks 3–5 built — no restructuring,
every hook is 1–3 lines. Each layer records what *it* owns: the API records
requests and admission; the worker records lifecycle and leases; the agent
records steps and resumes; the LLM/rate-limit boundary records calls, tokens,
waits, and errors.

### Files

| File | Change |
|---|---|
| `internal/metrics/metrics.go` | `New` (Phase 1) is the source; each process picks the metrics it owns |
| `cmd/orchestrator/main.go` | `m := metrics.New("forge_api")`; pass into router + handlers |
| `cmd/worker/main.go` | `m := metrics.New("forge_worker")`; pass into worker, agent, rate-limited backend |
| `internal/api/router.go` | `MetricsMiddleware(m)` wraps the router; `NewRouter(store, maxPending, m)` |
| `internal/api/handlers.go` | `createJobHandler`: `JobsSubmitted`/`JobsRejected`; `healthHandler(store, m)` |
| `internal/worker/worker.go` | `New(store, handler, id, lease, concurrency, m)`; lifecycle counters in `executeWithLease`/`extenderLoop` |
| `internal/agent/agent.go` | `New(backend, registry, m)`; step counters/histograms in the step loop; `StepsResumed` on checkpoint resume |
| `internal/llm/ratelimited.go` | `NewRateLimitedBackend(inner, limiter, allowance, m)`; LLM + rate-limit metrics around `Complete` |
| `internal/api/middleware.go` (new) | `MetricsMiddleware` + `statusWriter` |

### Behavior

**API layer** — a `MetricsMiddleware` wraps the whole router:

```go
func MetricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            route := chi.RouteContext(r.Context()).RoutePattern() // "/jobs/{id}", "" on 404
            if route == "" { route = "unknown" }
            start := time.Now()
            sw := &statusWriter{ResponseWriter: w, status: 200}
            next.ServeHTTP(sw, r)
            m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).Inc()
            m.HTTPRequestDuration.WithLabelValues(r.Method, route).
                Observe(time.Since(start).Seconds())
        })
    }
}
```

`createJobHandler` records `JobsSubmitted.WithLabelValues(taskType)` on a 201
and `JobsRejected.WithLabelValues("capacity")` (or `"invalid"`) on a 429/400.
`listJobsHandler` refreshes `PendingJobs` (or a dedicated `CountPendingJobs`
call per poll — see Phase 3's `/health`).

**Worker layer** — `executeWithLease` is the lifecycle chokepoint:

```go
func (w *Worker) executeWithLease(ctx context.Context, job store.Job, epoch int) {
    m := w.metrics
    m.InFlightJobs.Inc()
    defer m.InFlightJobs.Dec()
    m.ClaimsTotal.Inc()
    start := time.Now()
    err := w.handler.Run(ctx, w.store, job, epoch)
    if err == nil {
        m.JobsCompleted.Inc()
        m.JobDuration.Observe(time.Since(start).Seconds())
    } else {
        deadLetter := job.AttemptCount+1 >= job.MaxAttempts // MarkFailed's terminal branch
        m.JobsFailed.WithLabelValues(strconv.FormatBool(deadLetter)).Inc()
    }
}
```

`extenderLoop` increments `LeaseExtensions` on each successful renewal — this
is the "the lease is alive" signal, and its absence is the *failure* signal
(U2), so the counter has a direct alerting story.

**Agent layer** — the step loop (already checkpointing every step) becomes the
per-step latency histogram's home:

```go
// inside the plan → tool_call → observe loop, per step:
m.StepsTotal.WithLabelValues(stepType).Inc()
stepStart := time.Now()
// ... run the step, RecordStep(...)
m.StepDuration.WithLabelValues(stepType).Observe(time.Since(stepStart).Seconds())
```

On resume, `last := s.GetLastCompletedStep(...)`; if `last > 0`,
`m.StepsResumed.Add(float64(last))` — a metric that directly measures how much
crash-recovery work the system *did*, which is Week 3's thesis made countable.

**LLM / rate-limit boundary** — `RateLimitedBackend.Complete` (Week 5) is
already the only path every LLM call takes. Extend its decorate flow:

```go
// after the reserve wait (if any):
m.RateLimitWaits.WithLabelValues(limiterName).Inc()
m.RateLimitWaitTime.WithLabelValues(limiterName).Observe(waited.Seconds())

// around the inner call:
m.LLMCalls.WithLabelValues(backend).Inc()
callStart := time.Now()
resp, err := r.inner.Complete(ctx, req)
m.LLMDuration.WithLabelValues(backend).Observe(time.Since(callStart).Seconds())
if err != nil {
    m.LLMErrors.WithLabelValues(backend, classify(err)).Inc()
} else {
    m.LLMTokens.WithLabelValues(backend, "prompt").Add(float64(resp.Usage.PromptTokens))
    m.LLMTokens.WithLabelValues(backend, "completion").Add(float64(resp.Usage.CompletionTokens))
}
```

`classify` lives in `internal/llm/errors.go` and maps to the bounded set
`timeout|rate_limit|auth|provider|network|internal`.

### Interview angle

Instrumenting at the *chokepoints* — not scattering `metrics.Inc()` calls
through every branch — is the design. Four wrappers cover 100% of the traffic
because Weeks 3–5 already funneled it through four narrow seams (router,
`executeWithLease`, the step loop, `RateLimitedBackend`). This is the same
reason the implementation plan's Week 5 decorator chose the LLM boundary: "the
layer that already has to see every call is the layer that should measure it."
And `StepsResumed` is a metric that only exists *because* of Week 3's fencing
and checkpointing — it proves the crash-recovery feature isn't just working, it
can tell you *how often* it saved work.

### Acceptance

- Submit one job → `curl /metrics | grep forge_` shows non-zero
  `forge_jobs_submitted_total`, `forge_llm_calls_total`, `forge_llm_tokens_total`,
  `forge_steps_total`, `forge_step_duration_seconds_bucket`, `forge_in_flight_jobs`.
- Kill a worker mid-job → `forge_steps_resumed_total` increments on the
  reclaimer, and `forge_jobs_failed_total{dead_letter="true"}` stays zero.
- No `{job_id}`-type unbounded label anywhere in the exposition.
- `chaos_test.go` stays **byte-identical** — instrumentation must not perturb
  the exactly-once invariants.

---

## Phase 3 — `/metrics` and enriched `/health`

### The port/aggregation decision: per-process `/metrics` + one proxy

The Week 5 seed serves `/metrics` **per-worker** on `METRICS_PORT` (9091).
The implementation plan's dashboard polls `/metrics` on the API server. Two
forces pull against each other:

- **Per-process endpoints** are what a real Prometheus deployment scrapes
  (each process is a scrape target with an `instance` label). They're the
  correct production shape.
- **One origin for the dashboard** avoids CORS and gives a single
  link-shareable URL.

**Decision: keep per-process `/metrics` (Prometheus-shaped), and add a thin
orchestrator proxy `GET /api/worker-metrics/{worker}` that reverse-proxies to
the configured worker metrics URLs.** The dashboard gets one origin (the
orchestrator); worker metrics stay per-process (production-shaped); and no
exposition text is merged — the proxy forwards each worker's bytes verbatim
and the dashboard renders per-worker. Merging Prometheus text (histogram
bucket families from N sources into one) is a correctness trap we deliberately
avoid.

The orchestrator learns worker addresses from a new env var
`WORKER_METRICS_URLS` (comma-separated `http://host:port`), not by guessing:
in docker-compose that's `worker-1:9091,worker-2:9091,…`; on the two-VM deploy
it includes VM-2's worker IPs. No service discovery to build, configurable for
any topology, and it doubles as the health-check list for `/health`.

### Files

| File | Change |
|---|---|
| `internal/api/router.go` | `GET /metrics` → `m.Handler()`; `GET /api/worker-metrics/{worker}` → proxy; enrich `/health`; serve dashboard static (Phase 4) |
| `internal/api/proxy.go` (new) | `workerMetricsProxy(urls map[string]string) http.HandlerFunc` with per-request timeout + error handling |
| `internal/store/store.go` | Add `CountActiveWorkers(ctx context.Context, within time.Duration) (int, error)` to `JobStore` |
| `internal/store/postgres.go` | `SELECT count(*) FROM workers WHERE last_heartbeat > now() - $1::interval` |
| `internal/api/handlers.go` | `healthHandler(store, m, uptimeStart)` returns DB status, workers online, pending, version, uptime |
| `cmd/orchestrator/main.go` | Read `WORKER_METRICS_URLS`; pass to router |
| `.env.example` / `docker-compose.yml` | Add `WORKER_METRICS_URLS` for the orchestrator; `METRICS_PORT` already on workers |

### Behavior

- **`GET /metrics`** (orchestrator, on the API port) → `promhttp` exposition of
  the API metrics. **`GET /metrics`** (each worker, on 9091) → the worker
  metrics. Both are scrape-ready for a real Prometheus later (Phase 6b).
- **`GET /api/worker-metrics/{worker}`** → reverse proxy to
  `WORKER_METRICS_URLS` entry `worker`; 10s timeout; `502` + a `worker=…`
  header line in the body if unreachable. The dashboard marks that worker
  "offline" instead of silently dropping it — *silence is not success*.
- **`GET /health`** becomes a real readiness probe:

  ```json
  {
    "status": "ok",
    "db": "ok",
    "workers_online": 3,
    "pending_jobs": 2,
    "version": "0.6.0",
    "uptime_seconds": 3661
  }
  ```

  `db` is a live `CountPendingJobs` call (cheap and proves connectivity);
  `workers_online` uses `CountActiveWorkers` with a 30s heartbeat window;
  `status` is `"degraded"` (still 200) if `db` fails or `workers_online == 0` —
  the probe stays up so an external checker can read *why*.

### Interview angle

Two defensible calls here. First, **the proxy instead of a merged endpoint**:
"Prometheus text is per-target; I proxy, I don't merge — merging histograms
client-side is where dashboards silently lie." Second, **the readiness probe
does real work**: `/health` isn't a liveness string, it queries the DB and
counts live heartbeats, so `status: degraded` means something actionable.
`CountActiveWorkers` reuses the heartbeat mechanism from Week 3 (U2) — worker
presence was already being tracked; this just *reads* it.

### Acceptance

- `curl localhost:8080/metrics | grep forge_` → non-zero counters/histograms
  after one job.
- `curl localhost:9091/metrics | grep forge_` (a worker) → worker metrics;
  `curl localhost:8080/api/worker-metrics/worker-1` → the same text verbatim.
- `/health` shows `db: ok`, `workers_online ≥ 1`, a real `pending_jobs`.
- Point `WORKER_METRICS_URLS` at a dead port → `/api/worker-metrics/worker-1`
  returns 502 with a body the dashboard can render as "offline".

---

## Phase 4 — Dashboard (`web/`)

### Goal

A live, link-shareable dashboard, plain HTML + JS, that proves "I can tell if
this system is unhealthy in production." It polls the **real** APIs
(`/api/jobs`, `/api/jobs/{id}/steps`, `/api/worker-metrics/{worker}`, and the
orchestrator `/metrics`) — nothing mocked, nothing hand-fed.

### Files

| File | Change |
|---|---|
| `web/index.html` (rewrite) | Layout: stat tiles, charts, job table, step timeline, worker panel |
| `web/dashboard.js` (new) | Poll loop, Chart.js wiring, `/metrics` text parsing, rendering |
| `web/style.css` (new) | Minimal dark-on-light styling; no framework |
| `web/vendor/chart.umd.js` (vendored) | Chart.js pinned locally — **no CDN** (self-hosted system must not depend on a third-party origin) |
| `internal/api/router.go` | `GET /dashboard` → `web/index.html`; `GET /static/*` → fileserver over `web/` |

### Behavior

- **Poll cadence:** every **5s**, parallel fetches of `/api/jobs` (list),
  `/api/worker-metrics/worker-1..N` (from `WORKER_METRICS_URLS` exposed via a
  tiny `GET /api/workers` endpoint that returns the configured worker names),
  and `/metrics`. The step timeline for the *selected* job polls
  `/api/jobs/{id}/steps` at 2s so recovery is visible live.
- **Stat tiles** (top row): total jobs, completed, failed/dead-letter, pending,
  active workers, rate-limit waits. Each tile is a live number; workers each
  carry an online/offline dot from the proxy reachability.
- **Charts** (Chart.js, parsed from the `/metrics` exposition text):
  - **Job status breakdown** — donut from `/api/jobs` counts by status.
  - **Per-step latency** — bar chart of the `forge_step_duration_seconds`
    histogram buckets (read the `_bucket` cumulative counts; render as a
    distribution).
  - **LLM tokens by backend** — stacked bar of `forge_llm_tokens_total{
    kind=prompt|completion}`.
  - **Rate-limit waits** — sparkline of `forge_rate_limit_waits_total`.
- **Job table** — `GET /api/jobs` with status color-coding; click a row to
  expand its **step timeline**: the `job_steps` rows rendered in order, each
  labeled with `step_type`, `worker_id`, `duration_ms`, and status. For a
  recovered job the timeline visibly shows step 1–2 on worker-1 then steps
  3–5 on worker-2 — the "demo's money shot" from `STANDOUT_UPGRADES.md`.
- **Live update semantics:** every tile/chart is re-fetched and re-rendered on
  each poll tick; a stale fetch (network error) marks the panel "stale" rather
  than freezing silently.

### The `/metrics` parsing decision

The dashboard parses the Prometheus exposition text with a **~40-line JS
function** (regex over `NAME{labels} VALUE` and `NAME_bucket{le} VALUE`
lines) rather than pulling in a metric client library. It only reads the ~15
`forge_*` metrics this dashboard owns, on a trusted same-origin endpoint. A
full PromQL client is the Phase 6b problem (when a real Prometheus exists);
this dashboard's contract is "parse what we export," which keeps `web/` as
plain HTML+JS as the implementation plan demands.

### Interview angle

"Plain HTML + JS polling the real APIs" is a *deliberate* constraint, not a
shortcut: the dashboard has **zero backend logic of its own** — it renders
what the API and metrics already expose, so it can never show data the API
doesn't have (the classic dashboard lie). The step timeline is the payoff of
Week 3's checkpointing and Week 4's `worker_id` attribution: a recovered job
is a *visual story* (`claim@worker-1 → steps → kill → reclaim@worker-2 →
resume`), not just a row that changed state. And vendoring Chart.js is the
self-hosting discipline: no CDN dependency on a box that's supposed to run on
free, sometimes-airgapped infra.

### Acceptance

- Visit `http://localhost:8080/dashboard` → tiles populate within one poll
  tick; charts render non-empty after a few jobs.
- Submit a job, expand its row → step timeline streams in live (2s poll).
- Kill a worker mid-job → the selected job's timeline shows the worker handoff
  and `StepsResumed` tile increments; the dead worker's dot goes red.
- `GET /dashboard` and `GET /static/*` are served by the orchestrator; no
  external requests in the browser network tab (fully self-contained).

---

## Phase 5 — Tests

### Goal

Observability code must be as provably correct as the queue code — a metric
that counts the wrong thing is worse than no metric. All tests stay under
`-race`, use `ManualClock` where timing matters, and never touch the global
registry.

### Files

| File | Change |
|---|---|
| `internal/log/config_test.go` (new) | `LOG_FORMAT`/`LOG_LEVEL` parsing; JSON handler emits parseable lines |
| `internal/metrics/metrics_test.go` (new) | Render correctness, cardinality guard, histogram math |
| `internal/api/metrics_test.go` (new) | `MetricsMiddleware` records method/route/status; `/health` shape; proxy behavior |
| `internal/llm/llm_test.go` (extend) | `RateLimitedBackend` metric side-effects on success/failure/wait |
| `internal/agent/agent_test.go` (extend) | Step metrics + `StepsResumed` on resume |

### Test table

| Test | Proves |
|---|---|
| `TestSetup_JSONHandler` | `LOG_FORMAT=json` → `logger` output parses via `encoding/json`; carries `service` |
| `TestSetup_LevelFilter` | `LOG_LEVEL=warn` suppresses Info, keeps Warn |
| `TestMetrics_RendersExposition` | `m.Handler()` body has `# HELP`/`# TYPE`, `_total` counters, `_bucket`/`_sum`/`_count` for histograms |
| `TestMetrics_NoGlobalRegistry` | two `New("a")`/`New("b")` instances are fully isolated (no cross-pollution) |
| `TestMetrics_LabelCardinality` | calling with `task_type`/`step_type`/`backend` from a bounded set never grows the label set (assert `Describe` output) |
| `TestMiddleware_RecordsRoutePattern` | request to `/jobs/<uuid>` labels `route="/jobs/{id}"`, **not** the UUID |
| `TestMiddleware_RecordsStatus` | 404 → `status="404"`, `route="unknown"` |
| `TestHealth_Shape` | `db: ok`, `workers_online`, `pending_jobs`, `version`, `uptime_seconds` present |
| `TestWorkerMetricsProxy_Offline` | dead upstream → 502 + offline marker in body |
| `TestRateLimitedBackend_MetricsOnSuccess` | fake success → `llm_calls_total` +1, `llm_tokens_total{kind=prompt/completion}` == `Usage` |
| `TestRateLimitedBackend_MetricsOnError` | fake failure → `llm_errors_total{kind=classify(err)}` +1, tokens unchanged |
| `TestRateLimitedBackend_MetricsOnWait` | exhausted bucket → `rate_limit_waits_total` +1, wait histogram observed (ManualClock) |
| `TestAgent_StepMetrics` | one job → `steps_total` counts match committed `job_steps` rows |
| `TestAgent_StepsResumed` | fake prior `plan` row → `steps_resumed_total` ≥ 1 |

`internal/worker/chaos_test.go` stays **byte-identical** — same as Phase 2.

### Acceptance

- `go vet ./...` && `go build ./...` && `go test -race ./...` green.
- `go test -race -count=5 ./internal/worker/...` green (chaos fuzz, unchanged).
- No test uses the global default registry or sleeps > ~50ms except
  explicitly-`ManualClock`-driven cases.

---

## Phase 6 — Stretch: U8 OpenTelemetry distributed tracing (`internal/trace/`)

Only if the core phases land clean. This is the week's headline upgrade and
the demo's money shot: a job killed mid-flight resumes on another worker and
the **whole flow is one trace**.

### The free-backend decision: an in-process span exporter to `slog`

U8's spec offers three export options: in-process collector, Tempo, or Jaeger.
Running Tempo/Jaeger as a *requirement* would make the core demo depend on
extra infra; a **custom in-process `SpanExporter` that writes each span as a
structured `slog` line** gives full trace visibility (IDs, parent, status,
duration, attributes) with zero infrastructure — the free-stack ethos taken to
its conclusion. The OTLP HTTP exporter is wired as an *optional* second
exporter for Phase 6b's Jaeger profile. One `internal/trace` package, two
exporters, one `TracerProvider`.

### Files

| File | Change |
|---|---|
| `go.mod` | `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/trace`, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` |
| `migrations/000005_trace_context.up.sql` | `ALTER TABLE jobs ADD COLUMN trace_context JSONB;` |
| `migrations/000005_trace_context.down.sql` | `ALTER TABLE jobs DROP COLUMN trace_context;` |
| `internal/trace/trace.go` (new) | `Setup(service string)`, `slogExporter`, optional `WithJaeger(endpoint)` |
| `internal/trace/exporter.go` (new) | `slogExporter` implementing `trace.SpanExporter` |
| `internal/store/store.go` | `SetTraceContext(ctx, jobID, epoch, tc json.RawMessage) error` on `JobStore`; `TraceContext json.RawMessage` on `Job` |
| `internal/store/postgres.go` | `UPDATE jobs SET trace_context = $3 WHERE id=$1 AND lease_epoch=$2` (fenced) |
| `internal/worker/worker.go` | Span "claim" at claim; extract trace context from `job.TraceContext` |
| `internal/agent/agent.go` | Job-level span "job.run"; span per step (`step.plan`, `step.tool_call`, `step.observe`); persist trace context |
| `internal/llm/groq.go`, `ollama.go` | Inject W3C `traceparent` into the LLM HTTP request from the span context |
| `cmd/worker/main.go` | `trace.Setup("worker-" + id)`; `defer` `TracerProvider.Shutdown()` |

### Behavior

**The cross-worker trace** (the demo):

```
claim@worker-1 (span "job.run" created, trace ctx persisted to jobs.trace_context)
  → step.plan        (child)
  → step.tool_call   (child)   ← KILL worker-1 here, step 2/5
→ lease expires → worker-2 reclaims
  → worker-2 reads jobs.trace_context, extracts the same trace ID
  → span "reclaim" (child of the same trace)
  → step.tool_call (3), step.observe (4), step.tool_call (5) (children)
  → job completes; trace ends with ONE trace ID spanning two workers
```

Concretely:

- On claim, the worker starts `job.run` with `tracer.Start(ctx, "job.run")`.
  If `job.TraceContext` is non-empty (this job was reclaimed), it
  `Extract`s it with `otel.GetTextMapPropagator()` and passes the remote
  parent to `tracer.Start` — same trace, continuing.
- The agent, per step, starts `step.<type>` as a child of `job.run`, records
  `step_type`, `step_number`, `worker_id` as span attributes, and ends it when
  `RecordStep` commits.
- After starting `job.run`, the worker persists the span context via
  `SetTraceContext` (fenced by epoch) — the seam that makes *recovery a
  continuation, not a restart*.
- Inside `RateLimitedBackend` (the LLM chokepoint), a `llm.complete` span is a
  child of the current step span; the backends inject W3C `traceparent` into
  the HTTP request so the *provider's* view (if it traces) links too.
- Every span ends as a structured `slog` line:

  ```
  msg="span" service=worker-1 trace_id=9f0c… span_id=3ab1… parent=… name=step.tool_call
  status=ok duration_ms=1420 step_number=3 step_type=tool_call worker_id=worker-2
  ```

  So even with zero infra, `grep trace_id` reconstructs the full cross-worker
  journey.

**Propagation choice:** W3C `tracecontext` (the standard `traceparent`
header) — not B3 — because it's the default the whole ecosystem converges on,
and it's what `otel.GetTextMapPropagator()` defaults to.

### Interview angle

This is U8's whole point: metrics say *that* a job was slow or retried; the
trace says *why*. "I can show you one trace ID that survives a `kill -9`,
because the trace context is **fenced-checkpointed with the job** — the same
`lease_epoch` mechanism that prevents double-execution also makes sure a
reclaimer continues the trace instead of starting a new one." The trace-context
column is a one-migration, fenced-write addition because Weeks 3–4 already made
checkpointing and fencing first-class.

### Acceptance (stretch)

- `kill -9` a worker mid-job → worker-2's spans show the same `trace_id`,
  `parent` chains from `job.run` through `step.plan`/`step.tool_call`, and
  `duration_ms` per span.
- `grep trace_id` in worker logs reconstructs `claim@A → steps → reclaim@B →
  resume → complete` under one ID (or, with Phase 6b, in the Jaeger UI).
- `000005` up/down round-trips cleanly; `SetTraceContext` is fenced (0 rows on
  stale epoch).
- All existing tests stay green — tracing is additive and never changes
  control flow.

---

## Phase 6b — Stretch: docker-compose observability profile

An optional `observability` compose profile so the *stretch* stack is
one command away, without touching the core compose:

```yaml
services:
  prometheus:
    image: prom/prometheus:latest
    profiles: ["observability"]
    ports: ["9090:9090"]
    volumes:
      - ./config/prometheus.yml:/etc/prometheus/prometheus.yml:ro
  jaeger:
    image: jaegertracing/all-in-one:1.62
    profiles: ["observability"]
    ports: ["16686:16686", "4318:4318"]
    environment:
      - COLLECTOR_OTLP_ENABLED=true
```

- `config/prometheus.yml` (new) — scrape `orchestrator:8080/metrics` and
  `worker-1..4:9091/metrics` every 15s, `job_name: forge`.
- `docker compose --profile observability up -d` → Prometheus at :9090,
  Jaeger at :16686; the dashboard stays the same (it polls the proxies), the
  observability stack is *additive*.
- With `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318`, `trace.Setup`
  registers the OTLP exporter alongside the slog exporter.

**Interview angle:** "I can demo with zero infra (`slog` spans) or stand up
the full Prometheus+Jaeger stack with one profile flag — the metrics were
Prometheus-shaped from day one, so adding a real scraper is config, not
code." This is the difference between a toy and "it plugs into the ecosystem."

---

## Phase 7 — Deploy + live demo over HTTPS

### VM steps

```bash
ssh ubuntu@4orge.duckdns.org -i ~/.ssh/forge_vm
cd ~/forge

git pull origin main
for f in migrations/*.up.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"; done

# in .env: LOG_FORMAT=json, WORKER_METRICS_URLS per topology, METRICS_PORT=9091
docker compose build orchestrator worker
docker compose up -d
```

### Demo

```bash
# 1. live dashboard (link-shareable)
open https://4orge.duckdns.org/dashboard

# 2. burst + observe (reuse Week 5's script; watch tiles/charts live)
bash scripts/burst_load_test.sh

# 3. crash-recovery money shot, now visible:
#    run scripts/cp_solve_agent_demo.sh; mid-run kill a worker container;
#    watch the selected job's step timeline hand off workers and StepsResumed increment
docker compose kill worker-2
```

### Acceptance

- `https://4orge.duckdns.org/dashboard` loads and shows real, live job
  activity — the implementation plan's Week 6 checkpoint ("a live,
  link-shareable dashboard showing real job activity").
- `https://4orge.duckdns.org/metrics` and `…/health` respond; worker proxies
  reachable.
- `LOG_FORMAT=json` in the VM env → logs are JSON (grep-able by `service`).
- Week 4 crash-recovery demo **still passes** (logs now show the cross-worker
  span/trace story); Week 5 burst test **still passes**.
- Evidence captured into `docs/week6_demo.md` in the same discipline as Week
  4's `docs/week4_demo.md`.

---

## Out of scope / seeds planted

- **Real Grafana + Alertmanager** — deferred to Week 8's "basic alerting" and
  the general "no paid infra" ethos. The `/health` degraded path and the
  metrics are designed so a trivial `watch curl /health` alerting script
  works without Grafana.
- **WebSockets for the dashboard** — 5s polling is honest, simpler, and
  cacheable; real-time push is a Week 9 polish item.
- **Distributed OTel collector / Tempo** — the in-process slog exporter and
  optional Jaeger cover the free stack; a collector adds infra with no demo
  value here.
- **U10 (Week 7) — deterministic-time simulation** — `Clock`/`ManualClock`
  (Week 5) is the seed; Week 6 only leans on it in tests, it doesn't expand it.
- **Retry/backoff observability (per-attempt traces)** — span attributes
  already record `attempt`; deeper per-attempt tracing is Week 7 hardening.

## Progress tracking

| # | Task | Area | Core/Stretch | Status |
|---|---|---|---|---|
| 6.0 | `internal/log/config.go` — `Setup(service)`, `LOG_FORMAT`/`LOG_LEVEL` | Logging | Core | ☑ |
| 6.1 | Wire `Setup` into both `cmd/*/main.go` | Logging | Core | ☑ |
| 6.2 | Add `prometheus/client_golang`; rework `internal/metrics` on custom `Registry`; migrate 4 Week-5 seed counters | Metrics | Core | ☑ |
| 6.3 | `LatencyBuckets`/`StepBuckets` constants + cardinality rules documented | Metrics | Core | ☑ |
| 6.4 | `MetricsMiddleware` (method/route/status) + `statusWriter` | API | Core | ☑ |
| 6.5 | Handlers: `JobsSubmitted`/`JobsRejected`; `PendingJobs` gauge | API | Core | ☑ |
| 6.6 | Worker lifecycle: claims/completed/failed/lease/in-flight/job-duration | Worker | Core | ☑ |
| 6.7 | Agent step loop: `StepsTotal`/`StepDuration` histogram/`StepsResumed` | Agent | Core | ☑ |
| 6.8 | `RateLimitedBackend`: LLM calls/duration/tokens/errors + rate-limit waits (required metrics param) | LLM | Core | ☑ |
| 6.9 | `classify(err)` bounded error categories in `internal/llm` | LLM | Core | ☑ |
| 6.10 | `GET /metrics` (API + workers), `GET /api/worker-metrics/{worker}` proxy, `WORKER_METRICS_URLS` | API | Core | ☑ |
| 6.11 | Enriched `/health` + `CountActiveWorkers` on `JobStore` | API | Core | ☑ |
| 6.12 | Dashboard rewrite: tiles, Chart.js charts, job table, step timeline, worker panel (vendored chart.umd.js) | Dashboard | Core | ☑ |
| 6.13 | Observability tests (log/metrics/middleware/health/proxy/LLM/agent) — `chaos_test.go` byte-identical | Tests | Core | ☑ |
| 6.14 | U8 OTel: `internal/trace`, slog span exporter, migration `000005_trace_context`, fenced `SetTraceContext`, spans claim/step/llm.complete, W3C `traceparent` inject | Tracing | Stretch | ☑ |
| 6.15 | docker-compose `observability` profile + `config/prometheus.yml` | Observability stack | Stretch | ☑ |
| 6.16 | Deploy to VM + live HTTPS dashboard demo, evidence in `docs/week6_demo.md` | Deploy | Core | ☑ |

---

## Week 6 checkpoint

The system is **observable**, and you can prove it:

- `LOG_FORMAT=json` gives parseable structured logs from every process; `LOG_LEVEL` flips verbosity by env.
- `curl /metrics` exposes jobs processed, failure rate, per-step latency
  histogram, active worker count, LLM spend, and rate-limit backpressure —
  all with bounded label cardinality.
- `https://4orge.duckdns.org/dashboard` is a live, link-shareable dashboard
  showing real job activity, a per-job step timeline, per-worker online/
  offline status, and the crash-recovery handoff *visually*.
- Stretch: a `kill -9` mid-job renders as **one OTel trace** spanning two
  workers — the Week 3 story, now visible.
- The Week 4 crash-recovery demo and Week 5 burst test **still pass**;
  `chaos_test.go` is byte-identical.

## Verification

- **Local:** `go vet ./...` && `go build ./...` && `go test -race ./...` &&
  `go test -race -count=5 ./internal/worker/...`.
- **Metrics:** start orchestrator + workers, submit a job, then
  `curl localhost:8080/metrics | grep forge_` — confirm non-zero
  counters/histograms/gauges (and `curl localhost:9091/metrics | grep forge_`
  on a worker).
- **Dashboard:** visit `http://localhost:8080/dashboard`, confirm tiles/charts/
  job timeline update on the 5s poll; kill a worker and confirm the offline dot
  + step-timeline handoff.
- **Logging:** run with `LOG_FORMAT=json LOG_LEVEL=debug`, pipe a log line
  through `jq -e .` — parseable JSON with `service`, `time`, `level`.
- **Live:** deploy to the Oracle VM; confirm
  `https://4orge.duckdns.org/dashboard`, `/metrics`, and `/health`; re-run the
  Week 4 crash-recovery demo and Week 5 burst test and confirm both still pass;
  capture evidence into `docs/week6_demo.md`.