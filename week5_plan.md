# Week 5 Plan — Cost-aware rate limiting at the LLM boundary

> Thesis: Week 4 made the agent **crash-recoverable**. Week 5 makes it
> **financially safe and production-shaped** — a shared cost-aware token budget
> enforced at the LLM call boundary, measured in **tokens** against the
> provider's real free-tier budget, not requests-per-second. **Backpressure
> replaces failure**: workers *wait* when the budget is dry, leases hold, jobs
> stay alive.

The textbook rate limiter counts requests at the API boundary. Real capacity
lives at the LLM call boundary and is denominated in tokens, not requests — a
single agent job can burn thousands of tokens across a handful of HTTP calls.
So Week 5 wraps the LLM call itself with a token-bucket limiter whose tokens
are the *estimated token cost* of the request, then reconciles against the
provider's *actual* usage after the call. That is the difference between a
limiter that protects your API and a limiter that protects your **budget**.

---

## Section 0 — Where we are now

### Completed so far

| Week | Thesis | Evidence |
|---|---|---|
| 1 | Job API + durable Postgres queue | `POST /jobs`, `GET /jobs`, chi router, `000001_initial` migration |
| 2 | Atomic claiming + fencing | `SELECT … FOR UPDATE SKIP LOCKED`, `lease_epoch` fence token, `000003_fencing_checkpoints` |
| 3 | Crash recovery | Self-renewing lease (`executeWithLease`/`extenderLoop`), reclaim of expired `running` jobs, `000004_step_worker_attribution`, invariant chaos test (`chaos_test.go`) |
| 4 | LLM agent + durable checkpoint loop | `LLMBackend` abstraction, `cp_solve` agent with `search_kb`/`run_tests`, `plan`/`tool_call` step protocol, **live HTTPS crash-recovery demo on 2026-07-31** (`scripts/cp_solve_agent_demo.sh`, two-worker trace, exactly-once steps) |

Week 4's live demo proved the strong case and the honest edge: once a `plan`
row commits, a reclaimer reuses it (zero LLM re-spend); if the kill lands while
the LLM HTTP call is in flight, the reclaimer calls the LLM again. Week 5 does
not need to touch any of that machinery — it sits on top of it.

### Inherited scaffold (usable as-is)

- **`Usage{PromptTokens, CompletionTokens}`** in `CompleteResponse` — the U9
  seed planted in Week 4. Ollama reports `prompt_eval_count`/`eval_count`;
  Groq reports `usage.prompt_tokens`/`usage.completion_tokens`. Both backends
  already populate it, so the limiter has real settlement data with **zero
  backend changes**.
- **`FakeBackend`** with scripted responses + `CallCount` — the U10 seed for
  deterministic, network-free tests of the limiter decorator and the agent.
- **`RecordStep` with `worker_id` attribution** — the exact-once step ledger the
  load test will count against.
- **Bounded per-worker concurrency** (`WORKER_CONCURRENCY` semaphore, U6) —
  per-worker throughput cap already exists; Week 5 adds the *shared*, *cost*-
  aware cap at the LLM layer that the semaphore can't express.
- **`retryTransient`** — the resilience half. Week 5 adds the *cost* half.
  Retry and rate limit are orthogonal: retry re-sends a failed call, rate limit
  decides whether a call may happen at all.

### Absent (greenfield for Week 5)

- **`internal/ratelimit/`** does not exist — the whole package is new.
- No **admission control** at `POST /jobs` — the pending queue grows unbounded.
- No **burst / load test** — nothing has ever exercised the system under a 30+
  job burst.
- No **Upstash Redis** or any distributed limiter.
- No **LLM-call ledger** — spend is invisible; `usage` is parsed and dropped.

---

## Time & scope

- **Budget:** ~10-12 focused hours.
- **Core (must):** Phase 0 (admission control), Phase 1 (limiter primitives),
  Phase 2 (`RateLimitedBackend` decorator), Phase 3 (wiring + env + compose),
  Phase 4 (tests), Phase 5 (burst load test + demo), Phase 7 (deploy + live).
- **Stretch (if time):** Phase 6 (LLM-call ledger, Upstash Redis distributed
  bucket, Prometheus seed).
- **Order dependency:** Phase 1 → Phase 2 → Phase 3 → Phase 4 → Phase 5 is a
  hard chain (each phase's tests build on the previous). Phase 0 (admission
  control) is **independent** and can land first or in parallel.

---

## Phase 0 — Admission control at the API boundary (backpressure)

### Goal

`POST /jobs` returns **HTTP 429** when the pending queue exceeds
`MAX_PENDING_JOBS` — graceful rejection instead of unbounded queue growth.
Admission control protects the **queue**; the rate limiter (Phase 1-3)
protects the **LLM budget**. The two are complements, and both are the
"backpressure replaces failure" thesis made concrete at two different layers.

### Files

| File | Change |
|---|---|
| `internal/store/store.go` | Add `CountPendingJobs(ctx context.Context) (int, error)` to the `JobStore` interface |
| `internal/store/postgres.go` | Implement as `SELECT count(*) FROM jobs WHERE status = 'pending'` |
| `internal/api/handlers.go` | In `createJobHandler`, check `CountPendingJobs` before `CreateJob`; return 429 when at capacity |
| `internal/api/handlers.go` | Wire `MAX_PENDING_JOBS` from env at handler construction |

### Behavior

- `MAX_PENDING_JOBS = 0` (default) → **disabled**, current behavior preserved.
- `MAX_PENDING_JOBS = N` → when `pending >= N`, reject with:
  ```json
  {"error": "queue at capacity", "pending": 3}
  ```
  and `Retry-After: 5` so well-behaved clients back off.
- Jobs below the limit proceed normally — no other behavior change.

### Interview angle

Admission control is the *load-shedding* half of backpressure: a queue is a
buffer that absorbs bursts, but an unbounded buffer just turns a spike into a
long tail. Rejecting early with a clear 429 + `Retry-After` lets the client
retry with exponential backoff, which is strictly better than accepting work
you can't schedule for minutes. This pairs with the limiter the way a circuit
breaker pairs with a bulkhead.

### Acceptance

- Submit jobs with the queue drained → all `201` as today.
- With `MAX_PENDING_JOBS` set and the queue full → `429` with the error body;
  `GET /jobs` still lists everything already accepted.
- `MAX_PENDING_JOBS=0` (default) → identical behavior to Week 4.

---

## Phase 1 — Rate limiter primitives (`internal/ratelimit/`)

### Limiter interface

```go
package ratelimit

// Limiter gates LLM spend. Reserve blocks until estTokens are available,
// then debits them; Settle reconciles against the actual usage afterwards.
type Limiter interface {
    Reserve(ctx context.Context, estTokens int) error  // blocks until tokens available
    Settle(ctx context.Context, actualTokens int) error // refund over-reserve
    Name() string
}
```

### `MemoryBucket` — in-memory token bucket

The first implementation is intentionally single-process: it proves the
semantics, runs under `-race`, and needs no external dependency.

```go
type MemoryBucket struct {
    mu         sync.Mutex
    rate       float64   // tokens/sec refill
    capacity   float64   // burst ceiling
    tokens     float64   // current balance
    lastRefill time.Time
    clock      Clock
}

// Clock is the U10 seed: a test can substitute a ManualClock to drive
// time deterministically without wall-clock sleeps.
type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
}

type WallClock struct{}

func (WallClock) Now() time.Time                  { return time.Now() }
func (WallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
```

- **`Reserve`:** lock, refill `tokens += min(capacity, elapsed*rate)`; if
  `tokens < est`, compute `wait = (est - tokens) / rate`, unlock, and block on
  `select { <-clock.After(wait): refill again; <-ctx.Done(): return ctx.Err() }`;
  on wake, refill once more, debit, return nil. Returns immediately when tokens
  suffice.
- **`Settle`:** `tokens = min(capacity, tokens + max(0, est - actual))` —
  refund only the *over-reserved* amount. If the actual exceeded the estimate
  (rare, but a long completion can do it), nothing is refunded and the
  overage was already accounted as risk by the reserve debit.
- Capacity 0 (or `rate <= 0`) → degenerate: `Reserve` blocks until ctx
  cancel (a "closed" limiter), which is a useful test and safety mode.

### `MultiLimiter` — compose N windows

```go
// MultiLimiter reserves only when ALL inner limiters succeed; Settle fans
// out to all of them. Composes TPM + TPD + RPM windows for real providers.
type MultiLimiter struct { limiters []Limiter }
```

Reserve tries each in order; on any failure it returns the error and *settles
back* the already-reserved limiters so no token is stranded. This makes it safe
to express "6000 tokens/min AND 50k tokens/day AND 30 req/min" as one limiter.

### Token estimation (`internal/llm/tokens.go`, NOT in ratelimit)

Lives in `internal/llm` to avoid an import cycle (the decorator in `llm` needs
it, and it only reads `llm.Message`):

```go
// EstimateTokens is a cheap upper-bound heuristic: ~4 chars/token for the
// whole prompt plus a completion allowance for the model's reply.
func EstimateTokens(messages []Message, completionAllowance int) int
```

`(total chars across all messages) / 4 + completionAllowance`. The estimate is
deliberately a *conservative* upper bound — over-reserving is refunded by
`Settle`, so the system converges to real spend while bounding worst-case cost
per call.

### Env knobs (read in `cmd/worker/main.go`, Phase 3)

| Env | Default | Meaning |
|---|---|---|
| `RATE_LIMIT_BACKEND` | `memory` | `off` \| `memory` \| `upstretch` |
| `RATE_LIMIT_TPM` | `6000` | tokens/min (Groq `llama-3.1-8b-instant` free tier) |
| `RATE_LIMIT_RPM` | `0` (disabled) | requests/min, optional second window |
| `RATE_LIMIT_COMPLETION_ALLOWANCE` | `512` | tokens budgeted for the model's reply |
| `UPSTASH_URL` | — | REST endpoint (stretch, Phase 6) |
| `UPSTASH_TOKEN` | — | REST bearer token (stretch, Phase 6) |

### Acceptance

- `go test -race ./internal/ratelimit/...` green.
- A reserve just under the bucket completes instantly; a reserve over it blocks
  and then completes once refill credits it.
- `ctx.Done()` interrupts a blocked reserve promptly (no leaked goroutine).
- `Settle` returns exactly the over-reserved surplus to the bucket.

---

## Phase 2 — `RateLimitedBackend` decorator (`internal/llm/ratelimited.go`)

### Goal

Wrap an inner `LLMBackend` so every `Complete` call first reserves the token
estimate and later settles the real usage — **transparently**, with **zero
agent code changes**. The agent keeps calling `a.backend.Complete()`; when the
budget is dry it just takes longer.

### The "estimate-then-reconcile" pattern

Think of it like a credit card **authorization** followed by **settlement**:

1. **Reserve** an upper bound (`est`) *before* the HTTP call — this is the
   authorization, and it bounds worst-case spend per call.
2. **Call** `inner.Complete`.
3. **Settle** the actual cost after — on success
   `actual = prompt_tokens + completion_tokens` (the over-reserved delta is
   refunded to the bucket); on any error `Settle(ctx, 0)` refunds the whole
   reservation, because the provider doesn't charge for a failed request.

Over time the bucket converges to real spend; at every instant it bounds the
damage a burst can do.

### Flow

```go
type RateLimitedBackend struct {
    inner     llm.LLMBackend   // the real Ollama/Groq/Fake backend
    limiter   ratelimit.Limiter
    allowance int              // completionAllowance for the estimate
}

func (r *RateLimitedBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
    est := EstimateTokens(req.Messages, r.allowance)
    start := time.Now()
    if err := r.limiter.Reserve(ctx, est); err != nil {   // may block
        return CompleteResponse{}, fmt.Errorf("rate limit: %w", err)
    }
    if waited := time.Since(start); waited > 500*time.Millisecond {
        slog.Warn("rate-limit backpressure",
            "wait", waited, "est_tokens", est, "backend", r.Name())
    }
    resp, err := r.inner.Complete(ctx, req)
    if err != nil {
        _ = r.limiter.Settle(ctx, 0)      // full refund — no charge for a failure
        return CompleteResponse{}, err
    }
    actual := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
    if err := r.limiter.Settle(ctx, actual); err != nil {  // refund over-reserve
        return CompleteResponse{}, err
    }
    return resp, nil
}

func (r *RateLimitedBackend) Name() string { return r.inner.Name() }
```

### Why the Week-3 lease architecture makes this safe (interview angle)

`executeWithLease` (Week 3) runs the job under a self-renewing lease: a
separate `extenderLoop` goroutine renews the lease every `lease/3` while the
agent blocks inside `Reserve`. So a 60-second limiter wait does **not** cause a
false reclaim — the lease stays alive the whole time. This is the moment the
whole Week 3 fencing story pays off: **blocking rate limiting is only safe
because the lease is decoupled from the executing goroutine.** Without it, a
long backpressure wait would look like a dead worker and trigger a reclaim
loop. Week 5 gets blocking semantics for free, on top of already-proven
fencing.

### Structured logging

The decorator logs at WARN only when `wait > 500ms`:
`"rate-limit backpressure wait=3.2s est_tokens=450"` — the "visible
backpressure in logs" the implementation plan asks for, without log spam
during normal operation. This is also the U8 span point (Week 6 OTel seed).

### Acceptance

- `Complete` over budget blocks, then succeeds once refill credits it.
- A cancelled context during the wait returns `ctx.Err()` promptly and makes
  **no HTTP call** (spy on `FakeBackend.CallCount`).
- Success settles with `prompt+completion`; failure refunds the full reserve.
- Agent test (`TestRateLimitedAgent_StillExactlyOnce`) proves U9 does not
  regress the U4/U7 exactly-once invariant under `-race`.

---

## Phase 3 — Wiring, env knobs, compose

### `cmd/worker/main.go`

After `llm.NewFromEnv()` and before `agent.New`, build the limiter from env and
wrap the backend:

```go
backend, err := llm.NewFromEnv()
// ...
limiter, err := ratelimit.NewFromEnv()                 // off | memory | upstretch
// ...
backend = llm.NewRateLimitedBackend(backend, limiter, completionAllowance)
// ... tools registry unchanged ...
agentHandler := agent.New(backend, reg)                // agent never knows about the limiter
```

`ratelimit.NewFromEnv()` reads `RATE_LIMIT_BACKEND`, `RATE_LIMIT_TPM`,
`RATE_LIMIT_RPM`, and constructs a `MemoryBucket` (or `MultiLimiter` when RPM
is also set, or a no-op when `off`). With `off` and with `memory` + a huge TPM,
behavior is indistinguishable from Week 4 — the switch is safe by construction.

### `docker-compose.yml`

Each of the four `worker-N` services gains:

```yaml
- RATE_LIMIT_BACKEND=${RATE_LIMIT_BACKEND:-memory}
- RATE_LIMIT_TPM=${RATE_LIMIT_TPM:-6000}
- RATE_LIMIT_RPM=${RATE_LIMIT_RPM:-0}
- RATE_LIMIT_COMPLETION_ALLOWANCE=${RATE_LIMIT_COMPLETION_ALLOWANCE:-512}
- UPSTASH_URL=${UPSTASH_URL:-}
- UPSTASH_TOKEN=${UPSTASH_TOKEN:-}
```

The `orchestrator` service gains `MAX_PENDING_JOBS=${MAX_PENDING_JOBS:-0}`.

### `.env.example`

Document the new knobs with defaults + one-line rationale each, in the same
style as the existing Week 4 block (`LLM_BACKEND`, `GROQ_API_KEY`, …).

### Acceptance

- `docker compose up` starts with defaults; workers select the `memory`
  limiter and log `rate_limiter=memory tpm=6000`.
- `RATE_LIMIT_BACKEND=off` + a `cp_solve` job → completes exactly as in Week 4.
- `RATE_LIMIT_TPM=10` + a multi-iteration `cp_solve` job → WARN backpressure
  lines appear in worker logs and the job still completes (throttled, not
  failed).

---

## Phase 4 — Tests

### `internal/ratelimit/ratelimit_test.go` (new)

| Test | Proves |
|---|---|
| `TestMemoryBucket_AllowsUnderCapacity` | sequential reserves ≤ capacity complete instantly |
| `TestMemoryBucket_BlocksWhenExhausted` | reserve > remaining blocks, then succeeds after refill (tiny bucket, ~10ms real wait) |
| `TestMemoryBucket_RefillRate` | rate math: refilled amount == elapsed × rate, capped at capacity |
| `TestMemoryBucket_CtxCancel` | cancel while blocked → `ctx.Err()` promptly, no leaked timer goroutine |
| `TestMemoryBucket_SettleRefunds` | reserve 100 → settle 40 → next reserve sees 60 refunded |
| `TestMultiLimiter_AllMustPass` | one exhausted inner limiter blocks the whole reserve; no token stranded when a later inner fails |
| `TestMemoryBucket_ManualClock` (U10 seed) | `ManualClock` drives `Reserve` deterministically without wall-clock sleeps |

### `internal/llm/llm_test.go` additions

| Test | Proves |
|---|---|
| `TestRateLimitedBackend_WaitsThenSucceeds` | tiny `MemoryBucket` + `FakeBackend`: first call passes, burst blocked, after settle passes |
| `TestRateLimitedBackend_SettlesWithActualUsage` | spy bucket sees settle value == `prompt+completion` from the fake response |
| `TestRateLimitedBackend_FailureRefundsAll` | inner returns error → settle 0 (full refund), `FakeBackend.CallCount` unchanged by the wait |
| `TestRateLimitedBackend_CtxCancelDuringWait` | cancel ctx during `Reserve` → error, no HTTP call (assert `CallCount == 0`) |
| `TestEstimateTokens` | chars/4 + allowance sanity, empty messages == allowance |

### `internal/agent/agent_test.go` addition

| Test | Proves |
|---|---|
| `TestRateLimitedAgent_StillExactlyOnce` | U7-style invariant test: `FakeBackend` + counting tool + `RateLimitedBackend` + seeded killer → assert liveness, exactly-once `plan`/`tool_call` commit, contiguous steps, two-worker trace, under `-race`. **Proves U9 doesn't regress U4/U7.** |

`internal/worker/chaos_test.go` stays **byte-identical** — Week 5 is purely
additive at the LLM layer and must not perturb the Week 3 crash-recovery
invariants.

### Acceptance

- `go test -race ./...` green.
- `go test -race -count=5 ./internal/worker/...` green (chaos fuzz, unchanged).
- No test sleeps longer than ~50ms except the explicitly-`ManualClock`-driven
  ones.

---

## Phase 5 — Load test + demo script (`scripts/burst_load_test.sh`)

### Goal

Prove the Week 5 checkpoint under real load: **under a burst, no crashes, no
dropped jobs, visible backpressure in logs, all jobs complete** — and when
admission control is on, clean 429s instead of 5xx.

### Script shape (same style as `cp_solve_agent_demo.sh`)

`set -euo pipefail`, `curl` + `python3` for JSON, explicit PASS/FAIL, env
overridable (`API_URL`, `BURST_N`, `MAX_PENDING_JOBS`).

1. **Burst submit:** `BURST_N` (default 30) jobs — a mix of `segment` jobs and
   `cp_solve` jobs — fired concurrently via `curl` + `xargs -P`.
2. **Assert responses:** every response is `201` or `429`; zero `5xx`, zero
   connection errors.
3. **Drain:** poll `GET /jobs` until every accepted job is terminal
   (`completed` or `dead_letter`); assert none are `pending`/`running` forever.
4. **Backpressure evidence:** grep worker logs for `rate-limit backpressure`
   WARN lines; if `RATE_LIMIT_TPM` is set low, expect ≥1 (assert presence, and
   print the count).
5. **Exactly-once spot check:** for the `cp_solve` jobs, assert contiguous
   `plan`/`tool_call` steps (reuse the trace assertion from the Week 4 demo).
6. **Admission control mode** (when `MAX_PENDING_JOBS` set): assert the 429s
   carry `Retry-After` and the documented error body.

Works with any backend — real Groq, Ollama, or `LLM_BACKEND=fake`.

### Acceptance

- `scripts/burst_load_test.sh` passes against the local compose stack.
- The same script passes over HTTPS on the deployed VM (Phase 7).
- Every 201 job reaches a terminal state; no 5xx anywhere in the run.

---

## Phase 6 — Stretch (ledger, distributed limiter, observability)

Only if the core phases land clean. Each item is independently shippable and
explicitly seeds a later week.

### 6a — LLM-call ledger

Migration `000005_llm_calls`:

```sql
CREATE TABLE llm_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID REFERENCES jobs(id),
    worker_id TEXT,
    backend TEXT,
    prompt_tokens INT,
    completion_tokens INT,
    estimated_tokens INT,
    latency_ms INT,
    error TEXT,
    created_at TIMESTAMPTZ DEFAULT now()
);
```

- **Store:** `RecordLLMCall(ctx, ...)`, `ListLLMCalls(ctx, jobID)`.
- **API:** `GET /jobs/{id}/llm_calls` — per-job spend is now answerable:
  "this job cost X prompt + Y completion tokens across Z calls."
- **Agent hook:** 1-2 lines at each `a.backend.Complete` call site in
  `internal/agent/agent.go` (the agent already has the `store` in `Run`).
  The decorator in Phase 2 is backend-agnostic; the ledger records the same
  settle data with job context.

### 6b — Upstash Redis distributed bucket (`internal/ratelimit/upstretch.go`)

The in-memory bucket is per-worker; with 4 workers the shared budget must live
in shared state. Upstash Redis is reachable over plain REST, so no new Go
dependencies:

- **Fixed-window token counter** via `INCR` + `EXPIRE`:
  `forge:limiter:{name}:{window-id}` where `window-id = unix_ts / windowSec`.
- Atomic check-then-increment via **Lua `EVAL`** so concurrent workers can't
  overshoot the window (the entire reason a distributed limiter needs atomicity).
- Two windows: `60s` (TPM) and `86400s` (TPD).
- `RATE_LIMIT_BACKEND=upstretch` selects it; `UPSTASH_URL`/`UPSTASH_TOKEN`
  configure it. `MultiLimiter` composes it with the local RPM bucket unchanged.

### 6c — Prometheus seed (`internal/metrics/`)

Minimal counters to make backpressure observable (full observability is Week 6):

- `forge_llm_calls_total{backend}`
- `forge_llm_tokens_total{backend, kind="prompt|completion"}`
- `forge_rate_limit_waits_total`
- `forge_rate_limit_wait_seconds`

Wired into `RateLimitedBackend` behind an optional `metrics` parameter; no-op
by default so the core phases stay dependency-free.

### Acceptance (stretch)

- `GET /jobs/{id}/llm_calls` returns the recorded call rows for a `cp_solve` job.
- Two workers with `RATE_LIMIT_BACKEND=upstretch` + tiny TPM never exceed the
  window across the fleet (testable with `RATE_LIMIT_TPM=50`).
- Prometheus endpoints expose non-zero counters after a burst run.

---

## Phase 7 — Deploy + live demo over HTTPS

### VM steps

```bash
ssh ubuntu@4orge.duckdns.org -i ~/.ssh/forge_vm
cd ~/forge

git pull origin main
for f in migrations/*.up.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"; done

# set RATE_LIMIT_* (+ MAX_PENDING_JOBS) in .env for the VM's Groq backend
docker compose build worker orchestrator
docker compose up -d
```

### Demo

```bash
bash scripts/burst_load_test.sh          # over https://4orge.duckdns.org
```

Assert PASS: all accepted jobs terminal, no 5xx, backpressure lines in worker
logs (low TPM), and — with `MAX_PENDING_JOBS` set — clean 429s under burst.

### Acceptance

- Live run over HTTPS, evidence captured into this document (like Week 4's
  `docs/week4_demo.md`).

---

## Out of scope / seeds planted

- **U8 (Week 6) — OTel tracing:** context propagation is already pristine
  (every `Complete` receives the job's ctx); the decorator's WARN log is the
  span point. Nothing more to do this week.
- **U10 (Week 7) — deterministic time:** the `Clock` interface in Phase 1 is
  the seed; `ManualClock` in tests proves the seams. The agent/worker layers
  keep using wall-clock semantics as today.
- **Replacing the retry client:** `retryTransient` stays. Retry (resilience)
  and rate limit (cost) are orthogonal layers around `LLMBackend`.

---

## Progress tracking

| # | Task | Area | Core/Stretch | Status |
|---|---|---|---|---|
| 5.0 | `CountPendingJobs` + 429 admission control at `POST /jobs` (`MAX_PENDING_JOBS`) | Backpressure | Core | ☐ |
| 5.1 | `internal/ratelimit/` — `Limiter` interface, `MemoryBucket`, `Clock` | Limiter | Core | ☐ |
| 5.2 | `MultiLimiter` composition (TPM + RPM) | Limiter | Core | ☐ |
| 5.3 | `EstimateTokens` in `internal/llm/tokens.go` | Cost model | Core | ☐ |
| 5.4 | `RateLimitedBackend` decorator (reserve → call → settle) | LLM boundary | Core | ☐ |
| 5.5 | Backpressure WARN logging in the decorator | Observability | Core | ☐ |
| 5.6 | Wire limiter + env knobs in `cmd/worker/main.go` | Wiring | Core | ☐ |
| 5.7 | Compose + `.env.example` for `RATE_LIMIT_*` / `MAX_PENDING_JOBS` | Config | Core | ☐ |
| 5.8 | `internal/ratelimit` unit tests (+ `ManualClock`) | Tests | Core | ☐ |
| 5.9 | Decorator tests in `internal/llm` (+ `TestEstimateTokens`) | Tests | Core | ☐ |
| 5.10 | `TestRateLimitedAgent_StillExactlyOnce` (U9 × U7 invariant) | Tests | Core | ☐ |
| 5.11 | `scripts/burst_load_test.sh` — burst 30, no 5xx, all terminal | Load test | Core | ☐ |
| 5.12 | LLM-call ledger migration + `GET /jobs/{id}/llm_calls` | Ledger | Stretch | ☐ |
| 5.13 | Upstash Redis distributed bucket (`RATE_LIMIT_BACKEND=upstretch`) | Distributed | Stretch | ☐ |
| 5.14 | Prometheus seed counters | Observability | Stretch | ☑ |
| 5.15 | Deploy to VM + live HTTPS burst demo | Deploy | Core | ☐ |

---

## Week 5 checkpoint

Under a burst of 30+ jobs against the deployed stack:

- **no crashes** (workers stay up, no panics);
- **no dropped jobs** (every accepted job reaches a terminal state);
- **visible backpressure** (`rate-limit backpressure` WARN lines, bounded
  pending queue);
- **all jobs complete** (throttled, never failed);
- admission control rejects cleanly with `429` + `Retry-After` when enabled;
- the token-bucket limiter throttles at the LLM boundary — **no provider 429s**,
  because the client-side budget stays under the provider's free-tier limit;
- the Week 4 crash-recovery story still holds under rate limiting
  (`chaos_test.go` unchanged, live demo still passes).

## Verification

- **Local:** `go vet ./...` && `go build ./...` && `go test -race ./...` &&
  `go test -race -count=5 ./internal/worker/...`.
- **Load:** `scripts/burst_load_test.sh` against the local compose stack.
- **Live:** `scripts/burst_load_test.sh` against `https://4orge.duckdns.org`,
  with evidence captured (same discipline as Week 4's `docs/week4_demo.md`).

