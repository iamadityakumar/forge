# Forge — Interview Q&A & Explanation Prep

> **Role-play context:** You are an interviewer who has just discovered this
> repository ("Forge"). You have scanned the repo, noticed a live deployment,
> a `kill -9` crash-recovery demo, fencing tokens, rate limiting, tracing, and
> an unusual amount of testing discipline. This document is the answer key: the
> questions you would naturally ask, and the explanations the candidate should
> give — design decisions, tradeoffs, achievements, problems faced, how they
> were tackled, technologies learned, and lessons.

---

## 0. Snapshot — What the interviewer sees

| Fact | Detail |
|---|---|
| One-liner | Self-hosted job orchestration for multi-step AI agent tasks, built to survive real worker crashes with exactly-once step execution, on $0/month infrastructure |
| Language | Go 1.25, stdlib-first (`net/http` + `chi`, `pgx`, `x/sync`, Prometheus client, OpenTelemetry SDK) |
| Core idea | Postgres is the queue **and** the source of truth; workers claim jobs via `SELECT … FOR UPDATE SKIP LOCKED`, hold fenced leases, checkpoint every step, and resume exactly-once after a crash |
| Headline demo | `kill -9` a worker mid-agent-loop → a different worker reclaims the job, rebuilds the LLM conversation from committed `job_steps`, and finishes with contiguous, duplicate-free step numbers |
| Live deploy | `https://4orge.duckdns.org` — Oracle Cloud Always Free VM + DuckDNS + Caddy auto-TLS; Docker Compose: Postgres + orchestrator + 4 workers |
| Observability | `slog` JSON/text logging, per-process Prometheus `/metrics`, worker-metrics proxy, enriched `/health`, OTel tracing (slog + OTLP/Jaeger dual export), zero-dependency HTML dashboard |
| Testing | `go test -race ./...` in CI against a real Postgres; invariant chaos test; deterministic virtual-time simulation suite |
| Docs | Week-by-week plans (`week1_plan.md`…`week7_plan.md`), `forge-implementation-plan.md`, `FORGE_COMPLETE_BUILD_PLAN.md`, `STANDOUT_UPGRADES.md` (U1–U10) |

---

## 1. The 60-second pitch (memorize this)

> "Forge is a self-hosted job orchestration engine that runs multi-step AI agent
> tasks across multiple workers with checkpointed crash recovery, cost-aware
> rate limiting, and live observability — deployed on entirely free
> infrastructure. Every LLM decision and tool execution is a durable, fenced,
> checkpointed row. If a worker is killed mid-job, another worker reclaims it,
> bumps a fencing token, rebuilds the conversation from the committed steps, and
> resumes from the last checkpoint — exactly once, with zero lost steps and no
> re-spend on already-committed LLM decisions. Cost is enforced at the LLM call
> boundary in tokens against the provider's real budget, not at the request
> boundary. I proved the exactly-once-under-crash invariant with a
> deterministic chaos test under the race detector."

---

## 2. Architecture walkthrough (for the whiteboard)

```
POST /jobs ──► Orchestrator (Go, chi) ──► jobs row (Postgres)
                    │
                    ▼
        Postgres: jobs / job_steps / workers / llm_calls
                    │        ▲
          SKIP LOCKED claim │ │ fenced checkpoints + lease renewals
                    ▼        │
        Worker fleet (worker-1..4, each bounded concurrency)
          loop: plan → tool_call → observe (agent) or segments (dummy)
                    │
                    ▼
        LLMBackend interface (Ollama | Groq | Fake)
            └─ RateLimitedBackend decorator (token bucket reserve → call → settle)
                    │
                    ▼
        Tools: search_kb (embedded KB), run_tests (sandboxed python)
```

- **Orchestrator** (`cmd/orchestrator/main.go`): HTTP API only. Submits jobs,
  serves traces, metrics, health, dashboard. Falls back to an in-memory store if
  `DATABASE_URL` is missing (demo mode).
- **Workers** (`cmd/worker/main.go` + `internal/worker/loop.go`): poll loop
  behind a weighted semaphore (`WORKER_CONCURRENCY`), heartbeat goroutine, and a
  per-job lease-extension goroutine. Registered handlers per `task_type`
  (`cp_solve` → agent; anything else → dummy segment handler for backwards
  compatibility).
- **Store layer** (`internal/store/postgres.go`): all state transitions are
  single fenced SQL statements; 0 rows affected → classified as
  `ErrFenced` / `ErrInvalidTransition` / `ErrNotFound`.
- **Agent** (`internal/agent/agent.go`): reconstructs the conversation from
  committed steps, commits `plan` rows before tool execution, `tool_call` rows
  after, and resumes without re-calling the LLM for a committed decision.

---
## 3. Question bank — by theme

### A. System design & architecture

**A1. "What is this project, in one minute?"**
Cover: the one-liner above; the three pillars — durable queue claiming, crash
recovery / exactly-once, cost-aware limiting + observability; the $0/month
constraint; the weekly thesis → demo rhythm. Mention the repo name and the
live URL.

**A2. "Why did you choose Postgres as the queue instead of Redis/RabbitMQ/SQS?"**
Strong answer:
- `SELECT … FOR UPDATE SKIP LOCKED` gives production queue semantics in SQL —
  the same pattern GitHub and Oban/GoodJob use — without running a second
  piece of infrastructure.
- Jobs and their checkpoints (`job_steps`) need ACID: the queue row, the
  fencing token, and the step rows must be consistent. One database gives you
  atomic claiming **and** durable progress records.
- JSONB gives schema flexibility where needed (payloads), while `status`,
  `priority`, `lease_epoch` stay strongly typed.
- Tradeoff acknowledged: Postgres isn't a fan-out/broadcast system; for very
  high throughput or pub/sub you'd add Redis/Kafka — but for this workload
  (hundreds of jobs, correctness-first) a single ACID store is the right
  simplicity.

**A3. "Walk me through the flow of a job from submission to completion."**
1. `POST /jobs` → optional admission check (`MAX_PENDING_JOBS` → `429` +
   `Retry-After`) → `INSERT` with optional idempotency key.
2. Worker poll loop runs `ClaimJob`: atomic `UPDATE … WHERE id IN (SELECT …
   FOR UPDATE SKIP LOCKED)` → `status='claimed'`, `lease_epoch++`,
   `attempt_count++`.
3. Worker calls `StartJob` (`claimed → running`, fenced), starts a lease
   extender goroutine (`lease/3` cadence), executes the handler.
4. Agent loop: each LLM decision commits a `plan` row; each tool execution
   commits a `tool_call` row (fenced, upsert by `(job_id, step_number)`).
5. On `finish` decision → `CompleteJob` (fenced) → `completed`, `completed_at`.
6. On error → `FailJob`: retry with backoff (`run_at` gate) or dead-letter.

**A4. "Where does the AI agent live, and why is that the right separation?"**
The orchestration layer (`store`, `worker`) never imports `llm`/`agent`/`tools`
— the agent is a `Handler` registered per `task_type` (`internal/worker/execute.go`),
and it uses the same `RecordStep` / `ListSteps` store API as the dummy segment
handler. This means crash recovery, fencing, leases, and retries are
LLM-agnostic; swapping Ollama → Groq → a future backend requires zero changes
to the orchestration. That separation of concerns is what makes it a systems
engineering project rather than an ML script.

**A5. "Why two LLM backends?"**
`LLMBackend` interface (`internal/llm/llm.go`) with `OllamaBackend`,
`GroqBackend`, and `FakeBackend` (tests). Ollama proves you can run inference
self-hosted with no vendor; Groq gives fast, reliable live demos on a free
tier; the fake gives deterministic tests. The abstraction means the
orchestration doesn't care which one is running — and tests never touch the
network.

**A6. "How does this scale, and what's the honest limit?"**
Horizontal: add workers (compose has 4; the primary deployment uses scaled containers),
each worker bounded by `WORKER_CONCURRENCY`; Postgres handles the claim
contention via SKIP LOCKED. The honest limits: a single Postgres is the
bottleneck; the distributed rate limiter (Upstash Redis) is what lets the fleet
share one budget; in-memory limiter is per-process only. Load-tested with
`scripts/burst_load_test.sh` — graceful backpressure instead of failure.

---

### B. Queue claiming & concurrency

**B1. "Explain the claim query line by line."**
Point at `internal/store/postgres.go` `ClaimJob`:
- `UPDATE jobs SET status='claimed', claimed_by=$1, lease_expires_at=$3,
  lease_epoch=lease_epoch+1, attempt_count=attempt_count+1, run_at=NULL`
- `WHERE id = (SELECT id … WHERE (status='pending' OR (status IN
  ('claimed','running') AND lease_expires_at < $2)) AND (run_at IS NULL OR
  run_at <= $2) ORDER BY priority DESC, created_at ASC FOR UPDATE SKIP LOCKED
  LIMIT 1) RETURNING *`.
Explain each piece: the subselect picks exactly one candidate; `FOR UPDATE`
locks the row; `SKIP LOCKED` means concurrent workers skip locked rows instead
of blocking — no double-claim, no serialization bottleneck; `lease_epoch + 1`
mints a fresh fencing token for the claimer; `RETURNING *` returns the claimed
job in one round trip.

**B2. "What breaks without SKIP LOCKED? Without the row lock at all?"**
- Without `SKIP LOCKED`: concurrent workers block on the same row — head-of-line
  blocking, terrible throughput.
- Without the row lock at all: two workers can select the same job and both
  claim it → double execution.
This is the isolation-level / concurrency-control conversation: you need the
row lock for correctness and SKIP LOCKED for throughput.

**B3. "How do two workers avoid claiming the same job?"**
The claim is a single atomic `UPDATE` — Postgres row-level locking guarantees
exactly one claimant. `FOR UPDATE SKIP LOCKED` makes that both safe and
non-blocking. No distributed lock, no leader election needed at claim time.

**B4. "What is the job state machine?"**
`pending → claimed → running → completed` (or `failed`). `claimed` means
"leased but not yet started", `running` means "executing, checkpoints may
exist". Recovery paths: expired `claimed`/`running` → reclaim to `claimed`;
`failed` with attempts left → `pending` + `run_at` (scheduled retry);
`failed` with `dead_letter=true` → terminal.

**B5. "How do you bound concurrency per worker?"**
`golang.org/x/sync/semaphore` weighted by `WORKER_CONCURRENCY`
(`internal/worker/loop.go`). Each job gets its own lease-extender goroutine and
fenced step loop rooted in one cancellable context — structured concurrency:
on shutdown, the semaphore acquisition fails, `wg.Wait()` drains in-flight
jobs, and lease goroutines tear down via context cancellation.

**B6. "How does the heartbeat work and why?"**
Every worker runs a heartbeat goroutine (`internal/worker/loop.go`,
`heartbeatInterval = 10s`) upserting `workers.last_heartbeat`; `/health` and
`CountActiveWorkers` use "heartbeat within 30s" as liveness. **But** per-job
progress is governed by the lease, not the heartbeat — see C2.

---

### C. Crash recovery & exactly-once (the thesis)

**C1. "Walk me through the `kill -9` recovery demo end to end."**
Use `docs/week4_demo.md` and `scripts/cp_solve_agent_demo.sh`:
1. Submit a `cp_solve` job; worker A claims it (epoch n) and runs the
   plan → tool_call loop, committing durable `plan`/`tool_call` rows.
2. `kill -9` worker A mid-loop (the script hunts for the window between a
   committed `plan` and its `tool_call`).
3. Job sits in `running` until the lease expires (lease renewal stops — the
   process is dead).
4. Worker B's claim subselect sees `status='running' AND lease_expires_at <
   now()`, reclaims (`running → claimed`, `lease_epoch = n+1`).
5. B reads `job_steps` via `ListSteps`, `reconstructHistory` rebuilds the LLM
   conversation; a trailing lone `plan` with `action:"tool"` means B executes
   the tool **without** calling the LLM again — the money shot: zero LLM
   re-spend for the committed decision.
6. B commits the remaining steps (contiguous numbers, no duplicates), then
   `CompleteJob` with epoch n+1.
Live evidence in the docs (2026-07-31): 9 steps, workers `worker-1` → `worker-3`.

**C2. "Lease renewal as heartbeat — explain the design."**
A fixed lease breaks on long jobs (a healthy worker's lease expires mid-job →
false reclaim → double execution). A very long lease trades correctness for
slow recovery. The fix (`internal/worker/loop.go` `extenderLoop`): a per-job
goroutine renews `lease_expires_at = now() + lease` every `lease/3`, fenced by
epoch. Alive worker → renews → no false reclaim. Dead worker → stops renewing →
lease expires → reclaim. Zombie (deposed) worker → renewal returns 0 rows →
`ErrFenced` → cancels the job immediately. This collapses "worker presence" and
"job ownership" into one mechanism — same heartbeat/lease duality as Temporal
activity heartbeats and Kafka `max.poll.interval.ms`.

**C3. "What are fencing tokens and why are they necessary?"**
`SKIP LOCKED` + lease prevents two workers **claiming** the same job, but not
two workers **executing** it: a frozen (not dead) worker A can thaw after B
reclaimed, still believing it owns the job. Without a fence it would re-run
steps, collide on checkpoints, and race `CompleteJob`. Fix: `lease_epoch` is
incremented on every claim and treated as a fencing token — every mutation
(`StartJob`, `RecordStep`, `RenewLease`, `CompleteJob`, `FailJob`) runs
`WHERE lease_epoch = $mine`. Deposed A's writes affect 0 rows → `classifyZeroRows`
→ `ErrFenced` → A abandons. Double execution is prevented by construction, not
luck. Cite Kleppmann's fencing tokens.

**C4. "What bug did you find by reading the actual claim query?"**
The textbook claim subselect reclaims only `pending` and expired `claimed`.
But `StartJob` moves jobs to `running`, so a worker killed **after** start
leaves the job `running` forever — unrecoverable. The reclaim condition now
includes `running`: `status IN ('claimed','running') AND lease_expires_at <
now()`. This single change makes the crash-recovery demo actually work
(`STANDOUT_UPGRADES.md` U3).

**C5. "How do you checkpoint, and how do you resume exactly once?"**
`RecordStep` (`internal/store/postgres.go`) is a fenced CTE: `WITH owned AS
(SELECT 1 FROM jobs WHERE id=$1 AND lease_epoch=$2 FOR UPDATE) INSERT INTO
job_steps … SELECT … FROM owned ON CONFLICT (job_id, step_number) DO UPDATE
… RETURNING id`. 0 rows → `ErrFenced`. Resume: `LastCompletedStep =
MAX(step_number) WHERE status='completed'`, then continue from +1. The
`ON CONFLICT` upsert makes step writes idempotent; the epoch check makes them
fenced. So recovery is **resumption**, not restart — WAL/replay semantics.

**C6. "Is it truly exactly-once? What's the honest edge case?"**
The guarantee: once a step row is committed, it is never executed twice and
never re-spent (LLM). The honest edge case: if the worker dies **while the LLM
HTTP call is in flight, before a `plan` row commits**, there is no durable
decision to reuse — the reclaimer calls the LLM again. The docs state this
explicitly. Also note the system gives at-least-once execution with
idempotent, fenced, deduplicated step commits — i.e., exactly-once *effects*
at the step level under crash.

**C7. "What happens if the worker is killed between committing a `finish`
decision and `CompleteJob`?"**
`reconstructHistory` returns a pending decision with `action:"finish"`; the
reclaimer returns nil immediately, and the worker loop transitions the job to
`completed`. The durable finish decision is reused — no LLM re-call.

**C8. "How did you make the recovery testable without a real kill?"**
The chaos test (`internal/worker/chaos_test.go`): N workers against a fake
store that faithfully emulates fencing + SKIP LOCKED + reclaim + lease expiry,
while a **seeded** pseudo-random killer cancels a random worker's context
(simulating `kill -9`) and spawns a replacement. It asserts three invariants:
liveness (every job reaches terminal), safety (per-`(job,step)` execution
counter ≤ 1; steps per job are exactly `{1..K}` with no gaps), and no panics /
no data races under `-race`. Run with `go test -race -count=5
./internal/worker/...`.

---

### D. Retries, backoff, dead-letter

**D1. "What happens when a job fails?"**
`FailJob` (fenced) reads `attempt_count` vs `max_attempts`:
- Attempts left → requeue: `status='pending'`, `claimed_by=NULL`,
  `lease_expires_at=NULL`, `run_at = now() + backoff`, `lease_epoch++`
  (so a zombie can't interfere), `error_message=reason`.
- Exhausted → `status='failed'`, `dead_letter=true`, surfaced via
  `GET /jobs?status=dead_letter`.

**D2. "Why exponential backoff with jitter?"**
Backoff = `base * 2^(attempt-1)` (base 2s, cap 5min) plus jitter — prevents
thundering-herd retry storms when a transient outage ends. `run_at <= now()` is
a gate in the claim subselect, so a requeued job isn't claimable before its
time — and the same column doubles as a scheduled-jobs feature for free.

**D3. "What's a poison message, and how do you handle it?"**
A job that always fails: without handling, it would be retried forever. Forge
caps attempts (`max_attempts`, default 3) and moves the job to the
dead-letter state with the error message preserved — SQS DLQ semantics.

**D4. "What about transient LLM errors specifically?"**
`retryTransient` in `internal/llm/llm.go`: 429 and 5xx retried with exp backoff
+ jitter (honoring `Retry-After`), network/timeout errors retried, terminal
errors returned immediately. `ClassifyError` maps failures to bounded metric
categories (`timeout`, `rate_limit`, `auth`, `provider`, `network`,
`internal`) — bounded cardinality for Prometheus.

---

### E. Agent & LLM integration

**E5. "How is RAG implemented, and how do you know retrieval is useful?"**

The agent's `search_kb` tool has two paths. In production, `cmd/ingest` splits
the Markdown competitive-programming knowledge base into 1,200-character
chunks with 200 characters of overlap, embeds them with Ollama's
`nomic-embed-text`, and stores 768-dimensional vectors in PostgreSQL/pgvector.
An HNSW cosine index supports top-five nearest-neighbor search. At query time,
the tool embeds the query, retrieves the chunks, and returns source/chunk
metadata to the agent. For offline tests, the same tool falls back to
case-insensitive keyword matching over embedded files, and a deterministic fake
embedding backend makes embedding tests reproducible.

The evaluation dataset has 40 labeled queries and 30 executable tasks.
`cmd/rag-eval` reports Recall@1/3/5, MRR, and test pass rate. It intentionally
fails when the live database, pgvector, or embedding service is unavailable,
so the README does not claim unmeasured retrieval quality. A separate Groq
benchmark measured a small latency comparison: with retrieval, p50 was
`0.761s` and mean throughput was `336.8 tokens/s`; without retrieval, p50 was
`0.775s` and throughput was `330.5 tokens/s` (two runs per condition,
256-token cap). Those are latency/token measurements only; the generic prompt
does not produce executable code, so its pass field is `null`.

**E6. "What are the RAG tradeoffs and remaining gaps?"**

The vector path improves semantic matching over keywords, while the fallback
keeps local tests and degraded environments usable. The tradeoff is an
external embedding dependency and a fixed 768-dimensional schema. Chunking
and overlap are simple and reviewable, but not yet adaptive to document
structure. Retrieval latency is exported as Prometheus data and is visible in
the tool-call step. The next instrumentation improvements are recording
embedding calls in `llm_calls` and adding a dedicated retrieval trace span.

**E1. "Describe the agent loop."**
`internal/agent/agent.go`: build messages (system prompt listing tools +
user prompt from job payload), call the LLM asking for strict JSON
(`{"thought","action","tool_name","tool_args"}` or
`{"thought","action":"finish","answer"}`), commit the decision as a `plan`
row, execute the tool if `action:"tool"`, commit the observation as a
`tool_call` row, append to messages, repeat until `finish` or `AGENT_MAX_STEPS`
(default 10).

**E2. "How do you force the model to return parseable JSON?"**
- `response_format: json_object` (Groq) / `format: json` (Ollama).
- `parseDecision` tries direct JSON, then extracts the first `{…}` block
  (models sometimes wrap output in prose), and validates the schema.
- If it still fails, a "nudge" retry sends the assistant's bad output back with
  an instruction to return only valid JSON, then parses again.
- Tool execution errors never crash the loop — they become observations the
  model can react to.

**E3. "What tools does the agent have, and how are they sandboxed?"**
`search_kb` (embeds `internal/tools/kb/*.md` strategy docs via `go:embed`,
keyword search) and `run_tests` (writes the model's Python solution to a temp
dir, executes per test case with a per-case context timeout, returns JSON
pass/fail per case). Platform-specific exec via build tags
(`run_tests_unix.go` / `run_tests_windows.go`). The worker image includes
`python3`.

**E4. "How do you track LLM spend?"**
Every call is recorded in the `llm_calls` table: backend, estimated tokens,
actual prompt/completion tokens, latency, error. `EstimateTokens` is a
conservative heuristic (~4 chars/token + 500 overhead + 100 floor) used for
pre-call reservation; actual usage comes back from the provider and is
reconciled via the limiter's `Settle(actual)`.

---

### F. Rate limiting & backpressure

**F1. "Where do you rate-limit, and why there?"**
At the **LLM call boundary**, not the job-submission boundary — because that's
where real capacity lives (provider token budgets, local model throughput). A
cheap summarization call and an expensive codegen call consume different
budgets. `RateLimitedBackend` decorates any `LLMBackend`: estimate tokens →
`Reserve` → wait if needed → call → `Settle(actual)` (refund unused estimate,
debit deficit).

**F2. "Explain the token bucket implementation."**
`MemoryBucket` (`internal/ratelimit/memory.go`): thread-safe bucket with
fractional refill (`maxTokens / refillPeriod` per second), continuous refill
on access, reservation with computed wait time, and a `Settle` callback that
refunds or debits after actual usage. `MultiLimiter` composes TPM and RPM
buckets — both must authorize; partial grants are rolled back; wait is the max
of the two.

**F3. "What about multiple workers sharing one budget?"**
In-memory buckets are per-process — a 4-worker fleet could overshoot the
provider budget 4×. The distributed option (`UpstashBucket`) uses Upstash
Redis over REST with an atomic Lua `EVAL` (check + `INCRBY` + `EXPIRE` on first
write) keyed by fixed window, so the whole fleet shares one counter.
`MultiLimiter` composes the shared TPM bucket with a local RPM bucket.

**F4. "Why tokens instead of requests per second?"**
Tokens model real cost — a provider's free tier is a token-per-minute budget,
not an RPS ceiling. Request-count limiting protects an API; token limiting
protects a budget. That's the difference between a toy queue and a system that
models real constraints.

**F5. "How do you handle admission control at the API?"**
`MAX_PENDING_JOBS` (env): before insert, count pending; at capacity →
`429` + `Retry-After: 5`. Backpressure over failure — the system rejects early
rather than dropping or overloading. Verified with `scripts/burst_load_test.sh`.

---

### G. Observability

**G1. "How would you know this system is unhealthy in production?"**
1. `/health` readiness probe: DB ping, active worker count, pending depth,
   version, uptime — stays HTTP 200 with `status:"degraded"` so an external
   checker can read *why* from the body.
2. Prometheus metrics at every boundary: HTTP latency by route pattern, jobs
   submitted/completed/failed (by dead-letter), per-step and per-LLM-call
   latency histograms, lease extensions, in-flight jobs, rate-limit waits.
3. Structured logs: `slog` text/JSON, every process tagged with `service`.
4. Traces: one trace per job, `reclaim` child span on handoff, `llm.complete`
   spans, W3C `traceparent` injected into provider requests; spans emitted as
   slog lines (zero infra) and optionally to Jaeger via OTLP.
5. Dashboard: polls the real APIs, worker status dots, step timeline with
   per-worker attribution across recoveries.

**G2. "Why Prometheus format if you weren't running Prometheus initially?"**
It's the standard, it's free, and it signals ecosystem literacy rather than a
homegrown metrics format. A `docker compose --profile observability` profile
adds Prometheus + Jaeger when wanted. Each process has its own registry (no
global registry) so tests stay isolated.

**G3. "How does tracing survive a worker handoff?"**
On first claim, the worker starts a `job.run` span and persists
`{trace_id, span_id, trace_flags}` into `jobs.trace_context` (JSONB, fenced).
On reclaim, `trace.ExtractContext` injects it as a *remote parent*, so the new
worker's `reclaim` span (and every subsequent step span) continues the **same
trace_id**. Cross-worker journey: `grep trace_id` over logs, or Jaeger UI.

**G4. "Why a dual exporter (slog + OTLP)?"**
Zero-infrastructure guarantee: every ended span becomes one structured slog
line regardless of setup — `grep trace_id` reconstructs a journey. OTLP is
best-effort (batched, background retry) when an endpoint is configured — an
unreachable collector must never take the process down.

---

### H. Testing & determinism

**H1. "How do you test time-dependent behavior without flaky sleeps?"**
A `Clock` interface (`internal/clock/clock.go` — `Now/After/NewTicker/Sleep`)
threaded through store, worker, agent, and ratelimit. `ManualClock` is a
min-heap virtual timer where `Advance(d)` fires due timers in order. All SQL
`now()` became a `$now` bind from the clock. `FakeBackend` scripts responses,
errors, and delays. The sim harness (`internal/sim/`) drives canonical
races in virtual time: lease-expiry-while-alive, fencing-token race, backoff
timing — `<100ms`, identical output run-to-run, `-race -count=10` green.

**H2. "What does the chaos test prove?"**
The project thesis as a passing test: liveness, exactly-once step execution
(per-`(job,step)` counter ≤ 1, contiguous step sets), and no data races — under
`-race`, repeated `-count=5` with a seeded random killer. The fake store is the
oracle; it mirrors `postgres.go`'s invariants exactly.

**H3. "How does CI work?"**
GitHub Actions (`ci.yml`): a real `postgres:15-alpine` service container,
migrations applied in order via psql (hard DB-reachability gate), `go vet`,
`go build`, `go test -race ./...`, plus `go test -race -count=5
./internal/worker/... ./internal/sim/...`. Concurrency group cancels
superseded runs. Store tests skip (not fail) when DB is unreachable.

---

### I. Deployment & infrastructure

**I1. "What's the deployment stack and why is it free?"**
Oracle Cloud Always Free ARM VM (Ubuntu) + Docker Compose (Postgres 15,
orchestrator, worker-1..4, optional Prometheus/Jaeger profile) + DuckDNS
free subdomain + Caddy for automatic HTTPS. Total: $0/month — no trials, no
credit card. Groq free tier for LLM inference.

**I2. "Why containers, and why one worker service per container?"**
Explicit `worker-N` services (not `deploy.replicas`) so each gets a distinct
`WORKER_ID`, which the claim query stamps on `claimed_by` and the trace uses to
prove a *different* worker resumed after a kill.

**I3. "Windows dev machine, Linux prod — what broke?"**
Platform-specific tool exec via build tags (`run_tests_unix.go` vs
`run_tests_windows.go`); Docker on Windows Desktop needs `host.docker.internal`
for Ollama; ARM64 native builds on the Oracle VM (multi-arch clean since
`CGO_ENABLED=0`). CI runs Linux so the unix path is what's tested.

---

### J. Tradeoffs & Lessons

**J1. "What's the biggest tradeoff in this design?"**
Postgres-as-queue: simplicity + ACID correctness vs. throughput ceiling and no
native fan-out. For this workload, correctness first; if throughput became the
constraint, move to a real broker or shard.

**J2. "What did this project actually achieve?"**
- A live, link-shareable HTTPS system on genuinely free infra.
- Real, captured crash-recovery evidence: `kill -9` → different worker resumes,
  exactly-once, zero LLM re-spend for committed decisions.
- The exactly-once-under-crash invariant expressed as a passing `-race` chaos test
  plus a deterministic simulation suite.
- Cost-aware limiting at the LLM boundary with a token ledger (`llm_calls`).
- Observability depth: structured logs, Prometheus metrics, OTel tracing with
  cross-worker trace continuity, and a dashboard.

**J3. "What's the biggest lesson you'd pass on?"**
- **Prove claims with tests, not anecdotes** — the chaos test turns "exactly
  once under crash" from a story into a mathematical property.
- **Determinism beats flakiness** — inject the clock; test in virtual time.
- **Free infrastructure can still run production patterns** — the constraint
  forced creativity, not shortcuts.
- **Every line should be defensible** — minimal deps, stdlib-first, and each
  mechanism named (fence, lease, DLQ, backpressure) made the project a
  conversation rather than a code dump.
