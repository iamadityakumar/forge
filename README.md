# ⚡ Forge — Distributed, Crash-Resilient AI Agent Job Orchestrator

[![Go Version](https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go)](https://go.dev/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-15-4169E1?style=flat-square&logo=postgresql&logoColor=white)](https://www.postgresql.org/)
[![License](https://img.shields.io/badge/License-MIT-green?style=flat-square)](LICENSE)
[![Deployed on Oracle Cloud](https://img.shields.io/badge/Deployed-Oracle%20Cloud%20Always%20Free-F80000?style=flat-square&logo=oracle)](https://4orge.duckdns.org/dashboard)
[![Observability](https://img.shields.io/badge/Observability-OpenTelemetry%20%2B%20Prometheus-orange?style=flat-square)](https://4orge.duckdns.org/dashboard)

> **Self-hosted, cost-aware AI agent job orchestration system built in Go and PostgreSQL — engineered to survive real-world distributed failure modes (such as `kill -9` worker crashes mid-loop) with zero step loss, zero duplicate tool execution, token-denominated rate limiting, and live end-to-end observability on a $0/month infrastructure budget.**

---

## 📑 Table of Contents

- [The Core Problem & Philosophy](#-the-core-problem--philosophy)
- [System Architecture](#-system-architecture)
- [Key Engineering Upgrades (Beyond Textbook Queues)](#-key-engineering-upgrades-beyond-textbook-queues)
- [The Agent Loop & Crash Recovery](#-the-agent-loop--crash-recovery)
- [Cost-Aware Rate Limiting](#-cost-aware-rate-limiting)
- [Observability & Live Dashboard](#-observability--live-dashboard)
- [API Reference](#-api-reference)
- [Quickstart & Local Setup](#-quickstart--local-setup)
- [Deployment (Oracle Cloud Always Free)](#-deployment-oracle-cloud-always-free)
- [Testing & Invariant Verification](#-testing--invariant-verification)
- [RAG Knowledge Base, Evaluation & Benchmarking](#-rag-knowledge-base-evaluation--benchmarking)
- [Repository Structure](#-repository-structure)

---

## 🎯 The Core Problem & Philosophy

Standard job queues follow a textbook pattern: **Claim $\rightarrow$ Execute $\rightarrow$ Complete**. 

However, multi-step LLM agents (Plan $\rightarrow$ Tool Call $\rightarrow$ Observe $\rightarrow$ Reflect) violate standard queue assumptions:
1. **Long Execution Durations**: Agent loops take minutes, causing static lease expiries.
2. **Expensive Side Effects**: LLM inference and external tool calls cost real money and tokens; re-running from scratch on worker failure is unacceptable.
3. **Zombie Worker Execution**: A worker hitting a network partition or GC pause can wake up and double-execute tool calls concurrently with its replacement.
4. **Token-Denominated Bottlenecks**: Providers enforce strict Tokens Per Minute (TPM) and Requests Per Minute (RPM) limits at the API boundary, which request-level queues fail to govern.

**Forge solves these fundamental challenges by treating every step of the agent loop as a durable, fenced, checkpointed transaction.**

---

## 🏗️ System Architecture

Forge operates as a decoupled, multi-process distributed architecture:

```mermaid
flowchart TD
    Client(["🌐 Client / HTTP API / Dashboard"])
    
    subgraph OrchestratorLayer ["Orchestrator (cmd/orchestrator)"]
        API["HTTP API (Chi Router)"]
        Admission["Admission Controller\n(MAX_PENDING_JOBS)"]
        Proxy["Metrics Reverse Proxy\n(/api/worker-metrics/{worker})"]
        DashServer["Static Dashboard Web Server"]
    end
    
    subgraph StorageLayer ["PostgreSQL 15 (Single Source of Truth)"]
        JobsTable[("jobs\n(Status, Fencing Epoch,\nLease, Payload)")]
        StepsTable[("job_steps\n(Durable Checkpoints,\nWorker Attribution)")]
        LLMCallsTable[("llm_calls\n(Latencies, Tokens,\nAudit Log)")]
        WorkersTable[("workers\n(Heartbeats, Fleet State)")]
    end
    
    subgraph WorkerFleet ["Worker Fleet (cmd/worker - worker-1 .. worker-N)"]
        W1["Worker 1 (Lease & Heartbeat Goroutine)"]
        W2["Worker 2 (Atomic Claim & Recovery)"]
        WN["Worker N (Multi-Step Agent Runner)"]
    end

    subgraph LLMBoundary ["LLM Provider & Tool Execution"]
        RateLimiter["RateLimitedBackend\n(Memory / Distributed Redis Bucket)"]
        Groq["Groq / Ollama API"]
        Sandbox["Tool Sandbox (run_tests, kb_search)"]
    end

    Client -->|POST /jobs\nGET /jobs/{id}\nGET /dashboard| API
    API --> Admission
    Admission -->|INSERT| JobsTable
    
    W1 & W2 & WN -->|SELECT ... FOR UPDATE SKIP LOCKED\nClaim & Reclaim| JobsTable
    W1 & W2 & WN -->|Fenced Checkpoint Commits| StepsTable
    W1 & W2 & WN -->|Record Heartbeat| WorkersTable
    W1 & W2 & WN -->|LLM Calls & Spend Audit| LLMCallsTable
    
    W1 & W2 & WN --> RateLimiter
    RateLimiter --> Groq
    W1 & W2 & WN --> Sandbox
    Proxy -->|Scrape /metrics| W1 & W2 & WN
```

---

## 🚀 Key Engineering Upgrades (Beyond Textbook Queues)

Forge is built around senior distributed systems patterns:

| Upgrade | Problem in Textbook Queues | Forge Solution & Engineering Mechanism |
| :--- | :--- | :--- |
| **Fencing Tokens (`lease_epoch`)** | A stalled worker that wakes up after lease expiry will double-execute and corrupt state. | Every claim increments `lease_epoch`. All database mutations include `AND lease_epoch = $epoch`. Deposed workers affect 0 rows, detect `ErrFenced`, and abort cleanly. |
| **Self-Renewing Lease Heartbeats** | Static timeout leases expire prematurely on complex multi-step agent runs. | Per-job background goroutine renews `lease_expires_at` every `lease / 3`. If a worker halts, lease elapses and auto-heals; if healthy, lease extends infinitely. |
| **Expired `running` Reclaim** | Queues only reclaim `pending` or `claimed` jobs, stranding crashed `running` tasks. | Atomic query reclaims both unstarted expired claims and mid-execution `running` jobs in one step. |
| **Durable Step Checkpointing** | Crashing mid-agent loop restarts the entire job from step 1, burning LLM tokens. | Every `plan` and `tool_call` commits immediately to `job_steps` with `(job_id, step_number)` uniqueness. A reclaimer rebuilds context and resumes from step $N+1$. |
| **Exponential Backoff + DLQ** | Broken or poison jobs loop infinitely, starving the queue. | Configurable max attempts with exponential backoff and randomized jitter; poison tasks transition to `dead_letter: true` with stored error telemetry. |
| **Cost-Aware Token Buckets** | Rate limiters count only Requests Per Second (RPS), ignoring token usage. | Dual-bucket token rate limiter (`MemoryBucket` / `UpstashBucket`) estimating prompt tokens, reserving against TPM/RPM limits, and reconciling actual provider usage. |
| **Deterministic Time Simulation** | Async tests rely on `time.Sleep()`, causing flakes and slow test runs. | Virtualized `Clock` interface (`ManualClock`) and `FakeBackend` allowing deterministic time advancing and instant execution in chaos test suites. |

---

## 🔄 The Agent Loop & Crash Recovery

The core agent workload is `cp_solve`, which solves competitive programming challenges autonomously:

```mermaid
sequenceDiagram
    autonumber
    participant W1 as Worker 1 (Original Claimer)
    participant DB as PostgreSQL Store
    participant LLM as LLM Backend (Groq / Ollama)
    participant W2 as Worker 2 (Reclaimer)

    W1->>DB: Atomic Claim (leases job, epoch=1)
    W1->>LLM: Complete(prompt) -> Plan (Thought, Action: "tool", Tool: "kb_search")
    W1->>DB: Checkpoint Step 1 (type: "plan", epoch: 1)
    W1->>W1: Execute kb_search("binary search")
    W1->>DB: Checkpoint Step 2 (type: "tool_call", epoch: 1)
    
    Note over W1: 💥 SIGKILL / kill -9 on Worker 1!
    Note over DB: Worker 1 heartbeat ceases; lease_expires_at passes

    W2->>DB: Reclaim Expired Job (leases job, epoch=2)
    W2->>DB: Load committed job_steps (Steps 1 & 2 retrieved)
    Note over W2: Reconstructs LLM chat history without re-spending tokens!
    W2->>LLM: Complete(history + observation) -> Plan (Action: "finish")
    W2->>DB: Checkpoint Step 3 (type: "plan", epoch: 2)
    W2->>DB: CompleteJob(id, epoch=2) -> status="completed"
```

### The Invariant Guarantee
- Steps $1 \dots N$ are committed contiguously.
- No step number is duplicated or skipped.
- Both `worker_1` and `worker_2` step attributions are recorded transparently in the trace.

---

## ⏱️ Cost-Aware Rate Limiting

Forge implements token-denominated rate limiting at the provider boundary:

```go
// RateLimitedBackend enforces token + request quotas before hitting providers
type RateLimitedBackend struct {
    backend      LLMBackend
    limiter      ratelimit.Limiter
    metricsStore *metrics.Metrics
}
```

1. **Token Estimation**: Inspects message character counts ($\approx \text{chars} / 4$) before network transmission.
2. **Pre-Allocation**: Reserves token capacity in the `MemoryBucket` (or distributed Upstash Redis bucket via REST).
3. **Backpressure & Wait**: If over capacity, yields and waits for token replenishment instead of firing failing API calls.
4. **Post-Call Reconciliation**: Reconciles the actual `usage.total_tokens` against the initial reservation, crediting surpluses or debiting deficits.
5. **Transient 429 Resilience**: Parses upstream provider `Retry-After` headers (supporting both milliseconds `ms` and seconds `s`) with jittered exponential backoff.

---

## 📊 Observability & Live Dashboard

Forge includes built-in, zero-external-dependency observability:

- **Live Dark-Mode Dashboard**: Accessible at `/dashboard`, powered by lightweight vanilla JS and Chart.js.
  - Real-time job state breakdown (Pending, Running, Completed, Dead-Letter).
  - Step execution duration histogram ($p50, p95, p99$).
  - LLM Token consumption chart grouped by backend (`groq`, `ollama`) and direction (`prompt`, `completion`).
  - Interactive multi-worker execution trace timeline displaying crash recovery attribution.
- **Prometheus Metrics**: Scraped from `/metrics` (orchestrator) and `/api/worker-metrics/{worker}` (workers):
  - `forge_jobs_total{status, task_type}`
  - `forge_job_step_duration_seconds{step_type}`
  - `forge_worker_llm_tokens_total{backend, kind}`
  - `forge_worker_rate_limit_waits_total{limiter}`
- **OpenTelemetry Distributed Tracing**:
  - Context propagation using W3C `traceparent` headers across worker boundaries.
  - Structured `slog` exporter for instant log correlation.
  - Optional OTLP exporter for Jaeger/Zipkin collector export.

---

## 📡 API Reference

| Endpoint | Method | Description | Sample Response / Status |
| :--- | :---: | :--- | :--- |
| `/jobs` | `POST` | Submit a new job to the queue | `{"id":"...","status":"pending"}` (`201 Created`) |
| `/jobs` | `GET` | List recent jobs with status & pagination | `[{"id":"...","status":"completed"}, ...]` |
| `/jobs/{id}` | `GET` | Retrieve job details, error messages, attempts | `{"id":"...","status":"completed","attempt_count":1}` |
| `/jobs/{id}/trace` | `GET` | Get durable step execution history & worker IDs | `[{"step_number":1,"step_type":"plan","worker_id":"worker-1"}, ...]` |
| `/jobs/{id}/llm_calls` | `GET` | Audit log of all LLM calls, latencies & tokens | `[{"backend":"groq","prompt_tokens":418,"latency_ms":446}, ...]` |
| `/health` | `GET` | Readiness & health probe (DB, workers, queue) | `{"status":"ok","db":"ok","workers_online":4}` |
| `/api/stats` | `GET` | Aggregate fleet counts for dashboard | `{"total":1052,"completed":1046,"running":2}` |
| `/dashboard` | `GET` | Observability UI | Modern HTML5 / Dark-Mode Web Dashboard |

---

## 💻 Quickstart & Local Setup

### Prerequisites
- **Go**: Version `1.22+` or `1.25`
- **Docker & Docker Compose**
- **Optional**: [Ollama](https://ollama.ai/) running locally or a [Groq API Key](https://console.groq.com/)

### 1. Clone the Repository
```bash
git clone https://github.com/iamadityakumar/forge.git
cd forge
```

### 2. Configure Environment
```bash
cp .env.example .env
# Edit .env to set LLM_BACKEND=groq and GROQ_API_KEY=gsk_...
```

### 3. Launch with Docker Compose
```bash
docker compose build orchestrator worker-1 worker-2 worker-3 worker-4
docker compose up -d
```

### 4. Verify System Health & Dashboard
```bash
# Check health probe
curl -s http://localhost:8080/health

# Open dashboard in your browser
open http://localhost:8080/dashboard
```

### 5. Submit a Competitive Programming Job
```bash
curl -X POST http://localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "task_type": "cp_solve",
    "payload": {
      "prompt": "Given an array nums and target, find two indices that add up to target. Use two_sum(nums, target). Search KB, write solution, run_tests, then finish.",
      "language": "python"
    },
    "priority": 9
  }'
```

---

## ☁️ Deployment (Oracle Cloud Always Free)

Forge is designed to deploy on Oracle Cloud Infrastructure (OCI) ARM VM (`VM.Standard.A1.Flex` with 4 OCPUs, 24 GB RAM — 100% Always Free tier).

- **Production URL**: `https://4orge.duckdns.org/dashboard`
- **SSL / Ingress**: Caddy with automated Let's Encrypt certificates.
- **Runbook**: Detailed step-by-step instructions in [`docs/deploy_vm.md`](docs/deploy_vm.md).
- **Incident Troubleshooting**: Real incident playbooks in [`docs/troubleshooting.md`](docs/troubleshooting.md).

---

## 🧪 Testing & Invariant Verification

Forge maintains strict invariant tests and chaos verification:

```bash
# Run all unit and integration tests with the Go race detector
go test -race -v -count=1 ./...

# Run rate limit and worker chaos tests
go test -v ./internal/worker/ -run TestChaos
```

---

## 🔎 RAG Knowledge Base, Evaluation & Benchmarking

Forge includes a retrieval-augmented generation path for the competitive
programming agent. The knowledge base is stored in Markdown under
[`internal/tools/kb`](internal/tools/kb), and the same `search_kb` tool works
with either the production vector path or an offline fallback.

### RAG implementation

1. `cmd/ingest` reads Markdown files, splits them into 1,200-character chunks
   with 200 characters of overlap, embeds each chunk with Ollama
   (`nomic-embed-text` by default), and upserts it into PostgreSQL.
2. Migration `000008_kb_chunks` creates `kb_chunks` with a 768-dimensional
   `pgvector` column, a uniqueness constraint on `(source, chunk_number)`, and
   an HNSW cosine index.
3. `llm.EmbeddingBackend` abstracts embeddings. The repository provides the
   Ollama implementation for live ingestion and a deterministic fake backend
   for tests.
4. `search_kb` embeds the query, runs cosine-distance nearest-neighbor search,
   and returns the top five chunks with source and chunk metadata. Retrieval
   latency is recorded in the Prometheus metric
   `forge_retrieval_latency_seconds`.
5. If PostgreSQL or an embedding backend is not configured, `search_kb` falls
   back to case-insensitive keyword matching over embedded Markdown files. This
   keeps agent and unit tests runnable offline; production workers use vectors.

Current follow-ups are to record embedding calls in `llm_calls` with
latency/token metadata and to add a dedicated retrieval trace span. Retrieval
is currently visible in the tool-call step and latency is already exported.

```bash
# Start PostgreSQL with pgvector, apply migrations, and ingest the KB.
docker compose up -d postgres
go run ./cmd/ingest -dir internal/tools/kb

# Run the retrieval/task evaluation and write a JSON artifact after Ollama is ready.
go run ./cmd/rag-eval --output eval-results.json

# Benchmark local Ollama models. Retrieval context is supplied explicitly.
python scripts/rag_benchmark.py --models llama3.1 qwen2.5:3b \
  --retrieval "Prefix sums answer static range sums in O(1) after O(N) preprocessing." \
  --output rag-benchmark.json
```

Recall@k counts a query as recovered when its labeled source appears in the
first k results; MRR is the reciprocal rank of the first relevant source.
Pass rate is the fraction of dataset programs whose supplied tests pass.
The evaluator reports Recall@1/3/5, MRR, and executable task pass rate. Recall@k
is the fraction of labeled queries whose expected source appears in the first
k results; MRR is the mean reciprocal rank of the first relevant result. The
evaluator fails explicitly when PostgreSQL, pgvector, or Ollama is unavailable.
No live vector-evaluation artifact is checked in because the embedding service
was unavailable for the recorded environment run; the repository does not
invent recall or pass-rate numbers. The benchmark writes measured latency and
token fields only after its provider is available.

### Recorded Groq benchmark

The checked-in artifact
[`groq-qwen3.8-27b-rag-benchmark.json`](groq-qwen3.8-27b-rag-benchmark.json)
was produced with `qwen/qwen3.8-27b`, two requests per condition, a
256-token completion cap, and 12 seconds between requests:

| Prompt condition | p50 latency | mean throughput | runs |
| --- | ---: | ---: | ---: |
| Without retrieval | 0.775s | 330.5 tokens/s | 2 |
| With retrieval | 0.761s | 336.8 tokens/s | 2 |

The run used four requests, 1,024 completion tokens, and 154 prompt tokens in
total. This is a small latency/token measurement, not a statistically
definitive model comparison. `pass` is `null` because this generic prompt
benchmark produces prose rather than executable programs; executable task pass
rate belongs to `cmd/rag-eval`.

To reproduce the provider run, keep the API key outside tracked files:

```powershell
$env:GROQ_API_KEY = '<key supplied outside tracked files>'
python scripts/rag_benchmark.py --provider groq --models qwen/qwen3.8-27b `
  --retrieval 'Prefix sums answer static range sums in O(1) after O(N) preprocessing.' `
  --runs 2 --max-completion-tokens 256 --delay-seconds 12 `
  --output groq-qwen3.8-27b-rag-benchmark.json
```

---

## 📁 Repository Structure

```
forge/
├── cmd/
│   ├── orchestrator/          # HTTP API server, admission controller, metrics proxy
│   └── worker/                # Worker daemon, claim loop, agent execution engine
├── internal/
│   ├── agent/                 # Multi-step LLM agent protocol & decision parser
│   ├── api/                   # Chi HTTP handlers, router & reverse proxy
│   ├── clock/                 # Virtual Clock abstraction (SystemClock, ManualClock)
│   ├── llm/                   # LLM backends (Groq, Ollama, Fake) & RateLimitedBackend
│   ├── log/                   # Centralized structured slog logger
│   ├── metrics/               # Prometheus metrics registry & cardinality tracking
│   ├── ratelimit/             # Token bucket rate limiters (Memory, Upstash Redis)
│   ├── store/                 # PostgreSQL driver, migrations & atomic queries
│   ├── tools/                 # Agent tools (run_tests sandboxing, kb_search)
│   ├── trace/                 # OpenTelemetry tracing wrapper & W3C propagator
│   └── worker/                # Worker lifecycle, fencing tokens & claim manager
├── migrations/                # Versioned SQL migration scripts (000001 - 000007)
├── web/                       # Zero-dependency vanilla HTML/JS dark-mode dashboard
├── docs/                      # Deployment runbooks, demo scripts & troubleshooting guides
└── scripts/                   # Automated demo scripts & chaos verification suites
```

---

## 📜 License

This project is licensed under the MIT License — see the [LICENSE](LICENSE) file for details.
