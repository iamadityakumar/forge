# Week 6 Demo — Observability: Structured Logging, Prometheus Metrics, Live Dashboard, & OpenTelemetry Tracing

> Weeks 1–5 made Forge correct, atomic, crash-resilient, and cost-aware.
> Week 6 makes Forge **observable** — providing full visibility into system health, queue performance, per-step latency distributions, worker fleet state, and cross-worker tracing without extra paid infrastructure.

## What it proves

1. **Centralized Structured Logging (`internal/log`)**:
   - Environment-driven logging (`LOG_FORMAT=text|json`, `LOG_LEVEL=debug|info|warn|error`).
   - Every process tags log entries with its `service` name (`orchestrator`, `worker-1..N`).
2. **Boundary-Layer Prometheus Metrics (`internal/metrics`)**:
   - Per-process Prometheus 0.0.4 text exposition format on `/metrics`.
   - Bounded label cardinality: API route patterns (`/jobs/{id}`), bounded error categories (`timeout`, `rate_limit`, `auth`, `provider`, `network`, `internal`), and step types (`plan`, `tool_call`, `observe`).
   - Latency distributions: per-step duration histograms (`StepDuration`), HTTP request duration (`HTTPRequestDuration`), LLM call duration (`LLMDuration`).
   - Crash-recovery metrics: `StepsResumed` counter recording work saved after worker recovery.
3. **Metrics Proxy & Enriched `/health` (`internal/api`)**:
   - Orchestrator reverse proxies individual worker metrics via `/api/worker-metrics/{worker}`.
   - `/health` readiness probe returns DB connectivity, active worker fleet count (`CountActiveWorkers`), pending job queue depth, version, and uptime seconds. **Note:** The probe stays **200** even when degraded (status:"degraded") so an external checker can read *why*.
4. **Live Observability Dashboard (`web/`)**:
   - Zero-dependency plain HTML/JS dashboard styled with modern aesthetic dark-mode layout.
   - Polls `/api/jobs`, `/api/worker-metrics/{worker}`, and `/metrics` directly.
   - Interactive step timeline displaying live `worker_id` attribution across crash recoveries.
5. **U8 OpenTelemetry Distributed Tracing (`internal/trace`)**:
   - Backed by the **OpenTelemetry SDK** (`go.opentelemetry.io/otel` + `/otel/sdk` + `/otel/exporters/otlp/otlptrace/otlptracehttp`); the public API is a thin wrapper (`internal/trace`) keeping callers unchanged.
   - On claim, `trace.Setup` builds a `TracerProvider` with a **dual-exporter** design:
     - **`slogExporter`** (always): every ended span is emitted immediately as one structured `slog` line — zero infrastructure required, `grep trace_id` reconstructs a cross-worker journey.
     - **OTLP HTTP exporter** (when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, e.g. `http://jaeger:4318`): spans are batched and sent to a collector, so Jaeger shows the full cross-service trace UI. OTLP is best-effort — an unreachable endpoint retries in the background; slog spans always land.
   - On claim, `trace.Setup` starts a root `job.run` span; the span context (`trace_id`, `span_id`) is fenced-checkpointed into `jobs.trace_context` JSONB (migration `000006_trace_context`).
   - When a reclaimer extracts that context via `trace.ContextWithRemoteSpanContext`, it starts a `reclaim` child in the same trace — every subsequent step is a child under the same `trace_id`.
   - Inside the LLM chokepoint (`RateLimitedBackend.completeWithSpan`), an `llm.complete` span is a child of the current step span; the backends inject W3C `traceparent` via `trace.InjectW3C` (the OTel propagator) so the provider's view links too.
   - Every span ends as a structured slog line (`msg="span"` with `trace_id`, `span_id`, `parent_span_id`, `name`, `duration_ms`, `status`, and span attributes like `job_id`, `step_number`, `worker_id`, `backend`).
6. **Optional Prometheus docker-compose profile**:
   - Stand up Prometheus via `docker compose --profile observability up -d`.

---

## Verification & How to Reproduce

### 1. Unit & Integration Tests

```powershell
$env:GOFLAGS='-buildvcs=false'; $env:GOCACHE="$env:TEMP\go-build"; go test -timeout 30s -count=1 ./...
```

### 2. Structured Logging Verification

```bash
# Set JSON format and debug level
LOG_FORMAT=json LOG_LEVEL=debug go run ./cmd/orchestrator/main.go
# Expected line: {"time":"...","level":"INFO","msg":"orchestrator listening","service":"orchestrator"}
```

### 3. Prometheus Metrics & Worker Proxy Verification

```bash
# Query API metrics
curl -s http://localhost:8080/metrics | grep forge_

# Query Worker metrics via Orchestrator proxy
curl -s http://localhost:8080/api/worker-metrics/worker-1 | grep forge_
```

### 4. Readiness & Health Verification

```bash
curl -s http://localhost:8080/health
# Response shape:
# {
#   "status": "ok",
#   "db": "ok",
#   "workers_online": 4,
#   "pending_jobs": 0,
#   "version": "0.6.0",
#   "uptime_seconds": 120
# }
```

### 5. Live Dashboard Verification

- Visit `http://localhost:8080/dashboard` in any modern web browser.
- Verify that stat tiles update live, Chart.js metrics render, and selecting a job displays its step timeline.
- Simulate worker failure (`docker compose kill worker-2`) to observe the status dot turn offline and trace timeline update with cross-worker step attribution.

## VM Implementation (Oracle Cloud Always Free VM)

This section documents deploying Forge to the Week 6 stack on an Oracle Cloud Always Free ARM VM running Ubuntu. It assumes you already have:

- An SSH keypair at `~/.ssh/forge_vm` (already generated 2026-07-14, see `deploy/ORACLE_VM.md` for the public key).
- The public key pasted in the instance's SSH keys during VM provisioning.

### 1. Provision the VM (OCI Console)

Follow `deploy/ORACLE_VM.md` for the full console walkthrough, or quick steps:

1. Sign in to <https://cloud.oracle.com> → **Compute > Instances > Create instance**.
2. **Name:** `forge-vm`.
3. **Shape:** `VM.Standard.A1.Flex` (1 OCPU, 6 GB RAM).
4. **Image:** Ubuntu 22.04 LTS or 24.04 LTS (Canonical Ubuntu).
5. **Networking:** pick a VCN/subnet with a public IP; assign a public IPv4 address.
6. **Security list / NSG:** add ingress rules for ports 22 (SSH), 8080 (API), 80/443 (Caddy).
7. **SSH keys:** choose "Paste public key" and paste the contents of `~/.ssh/forge_vm.pub`.
8. **Boot Volume:** 50 GB is enough.

### 2. SSH into the VM and deploy

```bash
# From your local machine (no existing .env or build)
ssh -i ~/.ssh/forge_vm ubuntu@<VM_PUBLIC_IP>

# On the VM:
sudo apt-get update -y && sudo apt-get install -y git docker.io docker-compose
cd ~
REPO_URL=https://github.com/<your-org>/forge.git git clone $REPO_URL

cd forge

# Create a .env file based on .env.example:
# - Use the VM's internal Postgres DNS name (e.g. forge-postgres.<region>.oraclecloud.com)
# - Set LOG_FORMAT=json, LOG_LEVEL=info
# - Set WORKER_METRICS_URLS worker-1=http://worker-1:9091/metrics,worker-2=http://worker-2:9091/metrics,...
# - If you want OTel spans to reach Jaeger, set OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318 (requires the observability profile)

cat <<EOF > .env
DATABASE_URL=postgres://postgres:secret@postgres:5432/forge?sslmode=disable
LOG_FORMAT=json
LOG_LEVEL=info
WORKER_METRICS_URLS=worker-1=http://worker-1:9091/metrics,worker-2=http://worker-2:9091/metrics,worker-3=http://worker-3:9091/metrics,worker-4=http://worker-4:9091/metrics
# Optional OTLP export
OTEL_EXPORTER_OTLP_ENDPOINT=
EOF

# Build images (ARM64 native):
docker compose build

# Start the stack (Caddy will TLS-terminate on DuckDNS; run observability profile separately):
docker compose up -d
```

### 3. Verify the deployment

**From the VM (localhost):**

```bash
# API health
curl -s http://localhost:8080/health | jq .
# Expected: status:"ok", db:"ok", workers_online:4, pending_jobs:0, uptime_seconds >=0

# API metrics
 curl -s http://localhost:8080/metrics | grep forge_ | wc -l
# Expect >0 lines (jobs submitted, steps, llm calls, etc.)

# Worker metrics via orchestrator proxy
curl -s http://localhost:8080/api/worker-metrics/worker-1 | grep forge_ | wc -l

# Dashboard availability
curl -s -o /dev/null -w "dashboard HTTP %{http_code}, %{size_download} bytes\n" http://localhost:8080/dashboard
# Expect: HTTP 200, >5000 bytes
```

**From your laptop (over the public IP, with ports 8080/443 open):**

```bash
# Health probe
 curl -s http://<VM_PUBLIC_IP>:8080/health | jq .

# API metrics
curl -s http://<VM_PUBLIC_IP>:8080/metrics | grep forge_

# Dashboard (Caddy reverse proxy is configured to listen on 80/443)
open https://<VM_PUBLIC_IP>/dashboard
# Or curl -s http://<VM_PUBLIC_IP>/dashboard -o /tmp/dashboard.html
```

### 4. Demo the observability story

**Step 1: Burst load**

```bash
bash scripts/burst_load_test.sh
```

Wait ~2 minutes and watch tiles/charts update. The `forge_jobs_submitted_total`, `forge_jobs_completed_total`, per-step latency histograms, active workers, and rate-limit wait counters all rise.

**Step 2: Crash-recovery (the money shot)**

```bash
# Pick a job (find its ID from /api/jobs)
curl -s http://<VM_PUBLIC_IP>:8080/api/jobs | jq -r '.[0].id'  # pick the latest

# On the VM, kill worker-2 mid-job (the burst script spreads jobs across workers)
docker compose kill worker-2

# Watch the selected job’s step timeline unfold:
# - The status dot for worker-2 goes red in the dashboard
# - The job’s step timeline shows workers worker-1..N then a handoff back to worker-2
# - StepsResumed increments (grep for steps_resumed_total)
# - The trace timeline shows ONE trace_id surviving the kill and spanning two workers
```

### 5. Optional OTel stack (Observability docker-compose profile)

If you set `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4318` in `.env`, you can run:

```bash
docker compose --profile observability up -d
```

**On the VM**:

```bash
# Jaeger UI: http://<VM_PUBLIC_IP>:16686
# Prometheus UI: http://<VM_PUBLIC_IP>:9090
# Scrape targets are already in config/prometheus.yml (orchestrator:8080, worker-1..4:9091)
```

Verify spans appear in Jaeger: filter by the trace_id you saw in the dashboard step timeline; you should see the full job journey across workers.

### 6. Evidence capture checklist

- Screenshots of `/health`, `/metrics`, `/api/worker-metrics/worker-1`
- `curl http://<VM_PUBLIC_IP>:8080/dashboard` → save HTML; open in browser to confirm tiles/charts
- `docker compose logs orchestrator | jq -e .` for JSON-formatted logs
- For the crash-recovery, capture:
  - `/api/jobs` showing the job ID
  - step timeline UI after the worker kill
  - `forge_steps_resumed_total` metric increment (curl `…/api/worker-metrics/worker-1`)
- If OTLP is enabled, take a screenshot of Jaeger (trace ID from the job) and Prometheus (forge_jobs_submitted_total, etc.)

All artifacts are collected into `docs/week6_demo.md` as Markdown (copy-paste curl output, paste screenshots as markdown image links).

---

## Observability Architecture Summary

| Component | Responsibility | Endpoints / Location |
|---|---|---|
| `internal/log` | `slog` JSON/Text handler config & service context | Output: `os.Stdout` |
| `internal/metrics` | Typed Prometheus registry & histogram buckets | Orchestrator: `:8080/metrics`, Worker: `:9091/metrics` |
| `internal/api` | Middlewares, worker metrics proxy, enriched `/health` | `GET /metrics`, `GET /api/worker-metrics/{worker}`, `GET /health` |
| `internal/trace` | OTel-backed `traceparent`, span hierarchy, slog exporter, DB persistence | Migration: `000006_trace_context.up.sql` |
| `web/` | Plain HTML/CSS/JS frontend dashboard with vendored Chart.js | `GET /dashboard`, `GET /static/*` |

---

## Captured Evidence & Assertions

- All core & stretch tests pass (`go test -timeout 30s -count=1 ./...`).
- Exactly-once crash-recovery invariants verified (`internal/worker/chaos_test.go` logic unchanged; only mechanical interface/signature edits: `Run` gained a `*metrics.Metrics` param; fake store implements two new `JobStore` methods — invariants are byte-identical in spirit and pass `-race -count=3`).
- **OpenTelemetry-backed distributed tracing** (`internal/trace/trace_test.go`): W3C tracecontext, parent-child span linking, `ExtractContext`/`MarshalContext` round-trip, `InjectW3C`, and `slogExporter` line emission all verified. OTLP export is best-effort when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.
- **VM deploy runbook documented** in this section: everything needed to stand up the full stack (logging, metrics, dashboard, tracing, observability profile) on an Oracle Cloud Always Free VM, with verification steps and evidence checklist.

| Component | Responsibility | Endpoints / Location |
|---|---|---|
| `internal/log` | `slog` JSON/Text handler config & service context | Output: `os.Stdout` |
| `internal/metrics` | Typed Prometheus registry & histogram buckets | Orchestrator: `:8080/metrics`, Worker: `:9091/metrics` |
| `internal/api` | Middlewares, worker metrics proxy, enriched `/health` | `GET /metrics`, `GET /api/worker-metrics/{worker}`, `GET /health` |
| `internal/trace` | W3C `traceparent`, span hierarchy, slog exporter, DB persistence | Migration: `000006_trace_context.up.sql` |
| `web/` | Plain HTML/CSS/JS frontend dashboard with vendored Chart.js | `GET /dashboard`, `GET /static/*` |

---

## Captured Evidence & Assertions

- All core & stretch tests pass (`go test -timeout 30s -count=1 ./...`).
- Exactly-once crash-recovery invariants verified (`internal/worker/chaos_test.go` logic unchanged; only mechanical interface/signature edits: `Run` gained a `*metrics.Metrics` param; fake store implements two new `JobStore` methods — invariants are byte-identical in spirit and pass `-race -count=3`).
- **OpenTelemetry-backed distributed tracing** (`internal/trace/trace_test.go`): W3C tracecontext, parent-child span linking, `ExtractContext`/`MarshalContext` round-trip, `InjectW3C`, and `slogExporter` line emission all verified. OTLP export is best-effort when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.
