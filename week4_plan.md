# Week 4 — Agent Integration (the loop becomes an *agent*)

> **The thesis for this week.** Week 3 made a *job* crash-recoverable by checkpointing dummy
> segments. Week 4 makes the *agent* itself crash-recoverable: a real plan → tool-call → observe
> loop where each LLM decision and each tool execution **is a resumable, fenced step** in the
> U4 scaffold. `kill -9` a worker mid-LLM-call or mid-subprocess → a *different* worker reclaims
> → it **resumes from the last checkpointed decision**, never re-spending the LLM call that
> already committed → the job finishes with every plan/tool_call step executed **exactly once**.
> The agent is no longer bolted onto the queue; it *is* the step body.
>
> This week is driven by `STANDOUT_UPGRADES.md`. Week 4's stated mandate (the doc's ripple map)
> is: *"the LLM plan→tool-call→observation loop plugs into the checkpointed-step scaffold from
> U4 — each tool call IS a resumable, fenced step."* Weeks 5–7 then layer on top: **U9**
> cost-aware rate limiting (W5), **U8** OpenTelemetry tracing (W6), **U10** deterministic-time
> simulation (W7). This plan plants the *seeds* for each of those three so they plug in
> cleanly, but implements none of them.

---

## 0. Where we are now (progress snapshot)

| Area | Status | Evidence |
|------|--------|----------|
| **Week 1–3** — API skeleton, schema, `SKIP LOCKED` claim, fencing tokens (U1), reclaim-`running` (U3), checkpointed steps (U4), lease extension (U2), retry+DLQ (U5), bounded concurrency (U6), chaos test (U7), HTTPS deploy | ✅ done & on `main` | `cabe73b`/`8773a64` (PRs #1–#2 merged), CI-green; crash-recovery thesis demonstrated live over `https://4orge.duckdns.org` (2026-07-28, `DEMO_EXIT=0`) — see [`docs/week3_demo.md`](docs/week3_demo.md). |

### Week 4 — inherited scaffold vs. what's still absent

✅ **Done (inherited, untouched this week):**
- `internal/store` — the full fenced `JobStore` surface: `ClaimJob` (mints `lease_epoch`, reclaims `pending ∪ (claimed/running ∧ expired)`, gates `run_at`), `StartJob`/`CompleteJob`/`FailJob` (requeue vs. dead-letter), **`RecordStep`** (the fenced `WITH owned … FOR UPDATE` CTE + `ON CONFLICT` idempotent upsert), **`LastCompletedStep`** (`COALESCE(MAX(step_number),0) WHERE status='completed'`), `ListSteps`, `RenewLease` (self-fencing), `ErrFenced`.
- `internal/worker` — `Run` (bounded-poll), `runOneJob` → `executeWithLease` (spawns the U2 `extenderLoop` that renews every `lease/3` and self-cancels on `ErrFenced`) → `executeJob` (the checkpoint loop). `context.WithCancelCause` propagates fencing as a cancellation cause.
- `internal/api` — `GET /jobs/{id}/trace` (JSON array of `JobStep`, ordered by `step_number`). The `job_steps.step_type` column is already commented `-- plan | tool_call | observation` (migration `000001`); only `"segment"` is ever written today.
- `internal/worker/chaos_test.go` — `TestChaosRecoveryKillsExactlyOnce`: the U7 invariant test (liveness + exactly-once-commit + `-race`), seeded killer. **This file is the protected thesis test — it must stay byte-identical all week.**

🔴 **Not started — verified absent by reading `postgres.go`, `worker/*`, `api/`, `go.mod`:**

| Need | What the scaffold prepared | What is still missing |
|---|---|---|
| **task_type dispatch** | `executeJob` receives `job.TaskType` and ignores it | No registry / no `switch`; every job decodes only `{"segments":N}` from payload. Week 4 introduces `task_type → Handler`. |
| **LLM layer** | nothing | No `internal/llm/`; no `LLMBackend` interface; no `ollama.go`/`groq.go`; **no outbound `net/http.Client` anywhere in non-test code**; no LLM env knobs. |
| **Tools** | nothing | No `internal/tools/`; no `Tool` interface/registry; no sandboxed `exec`. The `step_type` vocabulary (`plan|tool_call|observation`) exists only as a SQL comment. `JobStep.Input` is left NULL (segments write only `Output`). |
| **Agent loop** | nothing | No `internal/agent/`; no plan→act→observe reconstruction. |
| **Per-step worker attribution** | steps are fenced-at-write but store no provenance | `job_steps` has **no `worker_id`** (and no `lease_epoch`) column → the trace cannot today attribute a step to a worker. The Week-3 demo narrated cross-worker execution but could only show it via job-level `claimed_by`. |
| **Deps plumbing** | `worker.Run(ctx, s, workerID, lease, concurrency)` is the only seam the chaos test touches | No way to hand LLM/tools/config to a handler. |

🟡 **Watch-outs inherited (must design around, not pretend away):**
- **`RecordStep` hardcodes `status='completed'`** — `step.Status` is *ignored*. There is no "in-progress step row" today; the checkpoint primitive only ever writes a completed row. (We accept this: the agent's atomic unit of progress is a *completed* step; an in-flight LLM call / in-flight subprocess that gets cancelled by ctx simply never commits.)
- **`ClaimJob` increments `attempt_count` with no `max_attempts` guard; dead-letter happens only inside `FailJob`.** A job that is repeatedly killed-and-reclaimed (never reaching `FailJob`) reclaim-loops forever. The agent self-limits this with `AGENT_MAX_STEPS → FailJob` (see Phase 3); segment jobs were never at risk (they're finite by `segments`).
- **No `Release` method** — abandon is by lease-expiry-→-reclaim, not an explicit requeue. We do not add one in Week 4.
- **No `Clock`/`FakeLlm` seam** — `computeBackoff` uses package vars (`backoffBase`/`backoffCap`/`backoffJitter`, `postgres.go`); the planned Clock + `FakeLlm` injection is explicitly deferred to **Week 7 / U10**. We *seed* it now with `llm.FakeBackend`.
- **`internal/ratelimit/` and `internal/metrics/` are ABSENT** — U9 (token-bucket) is Week 5, U8 (OTel) is Week 6. We add *neither*; we only keep `Usage` in the LLM response and keep ctx propagation pristine so both plug in later.

**Bottom line:** the checkpointing, leasing, and fencing are done and proven. Week 4 is almost entirely *greenfield packages* (`llm`, `tools`, `agent`) + one additive refactor to the dispatch seam + one tiny migration. The hard part is designing the **agent resume protocol** so a dynamic plan→act→observe loop maps onto Week-3's fixed-step-number resumption — which is what the rest of this plan resolves.

---

## Time & scope

**Budget:** ~10–12 focused hours. **Core (must finish):** Phase 0 (dispatch + `worker_id`), Phase 1 (LLM), Phase 2 (tools), Phase 3 (agent loop) + its thesis test, Phase 5 (wiring), Phase 7 (demo). **Stretch (nice, not blocking):** Phase 4 framing can pull forward, Phase 6 (agent-loop chaos test = a U10 predecessor, multi-language `run_tests`, sandbox hardening).

**Hard order dependency:** Phase 0 (the `worker.Handler` seam + `worker_id`) gates every later phase — the agent handler *is* a `worker.Handler`, and per-step `worker_id` powers the demo's money-shot. Phases 1→3 each depend on the prior only loosely (Phase 3 needs both 1 and 2), so 1 and 2 can interleave; Phase 5 (wiring) needs all three.

---

## Phase 0 — The dispatch seam + per-step `worker_id` *— the additive refactor everything plugs into*

> `executeJob` stops being "the segment loop" and becomes a **dispatcher**: one `task_type → Handler`
> lookup, where every step body is a `Handler` that owns its own checkpointing via the *unchanged*
> `RecordStep`/`LastCompletedStep` surface. The proven segment loop moves **verbatim** into a
> `segmentHandler` (so the chaos test, the live demo, and every existing test stay byte-identical),
> and `workerID` is threaded down to `RecordStep` so each checkpointed step records **which worker
> committed it**. This phase is functionally a no-op until an agent handler is registered in Phase 5
> — but it must land first, because the agent handler *is* a `worker.Handler`.

### Task 0.1 — Per-step worker attribution (`migrations/000004`, `models.go`, `postgres.go`)
**Files:** `migrations/000004_step_worker_attribution.{up,down}.sql`, `internal/store/models.go`, `internal/store/postgres.go`

```sql
-- migrations/000004_step_worker_attribution.up.sql
-- Week 4: per-step worker attribution. job_steps records which worker committed each
-- checkpoint so the agent trace can show a reclaimed job's plan/tool_call steps attributed
-- to TWO different workers — the cross-worker attribution the Week-3 demo narrated but
-- could only show via job-level claimed_by (the final owner).
ALTER TABLE job_steps ADD COLUMN worker_id TEXT;
```
```sql
-- migrations/000004_step_worker_attribution.down.sql
ALTER TABLE job_steps DROP COLUMN IF EXISTS worker_id;
```

```go
// internal/store/models.go — JobStep gains one field (additive; all readers unaffected).
type JobStep struct {
    // ... existing fields unchanged ...
    WorkerID   string          `json:"worker_id,omitempty" db:"worker_id"`
}
```

The `RecordStep` CTE gains `worker_id` (`$8`) using `NULLIF($8,'')` so the legacy test path
(which passes no worker id) stores NULL; the `ON CONFLICT` arm updates it too (a reclaiming
worker that re-commits an already-present step rewrites attribution correctly):

```sql
-- internal/store/postgres.go  RecordStep
WITH owned AS (
    SELECT 1 FROM jobs WHERE id = $1 AND lease_epoch = $2 FOR UPDATE
)
INSERT INTO job_steps (job_id, step_number, step_type, input, output, status, duration_ms, worker_id)
SELECT $1, $3, $4, $5, $6, 'completed', $7, NULLIF($8,'') FROM owned
ON CONFLICT (job_id, step_number) DO UPDATE
SET output = EXCLUDED.output, status = EXCLUDED.status,
    duration_ms = EXCLUDED.duration_ms, worker_id = EXCLUDED.worker_id
RETURNING id
```

`ListSteps` (the `GET /jobs/{id}/trace` query) adds `worker_id` to the `SELECT` and scans it via
`sql.NullString → st.WorkerID`. The `jobTraceHandler` JSON-encodes `JobStep` already, so
`worker_id` appears in the trace **for free** (omitted when empty) — no API change.

- **Why include now, not defer:** adding a nullable column + one struct field changes *no*
  `JobStore` method signature, so `chaosStore`/`memStore`/every Week-3 test stay compilable
  byte-for-byte (verified: `chaosStore.RecordStep` stores the whole `*store.JobStep`; the in-memory
  fakes never assert on `worker_id`). The agent-trace money-shot — "worker-2 reasoned steps 1–3,
  worker-1 reasoned steps 4–6" — needs this to be visible in the API. Low scope, high payoff.

**Acceptance:** a `segments` job run through the binary shows each row with `worker_id` populated in `GET /jobs/{id}/trace`; legacy tests (passing no `WorkerID`) store NULL and still pass. `go build ./... && go vet ./...` clean.

### Task 0.2 — Make `executeJob` a dispatcher; move the segment loop into a `segmentHandler`
**File:** `internal/worker/execute.go` (and one-line edits in `loop.go`, two test files)

```go
// internal/worker/execute.go
// Handler is a resumable, fenced, checkpointed executor for one task_type.
// Implementations bake their own dependencies (LLM, tools, config) at construction
// time (in cmd/worker/main.go) and do their own checkpointing via the UNCHANGED
// store.RecordStep / LastCompletedStep / ListSteps — the worker package never imports
// llm/tools/agent, so there is no import cycle.
type Handler interface {
    Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error
}

var (
    handlerMu sync.Mutex
    handlers  = map[string]Handler{}
)

// RegisterHandler binds a task_type → Handler. Called once at worker start (Phase 5).
func RegisterHandler(taskType string, h Handler) {
    handlerMu.Lock(); defer handlerMu.Unlock()
    handlers[taskType] = h
}

// lookupHandler returns the registered handler, else the built-in segmentHandler.
// Backwards-compat: unknown or legacy task_types — including the Week-3 "segments"
// jobs and the live kill-recovery demo — keep running as segments.
func lookupHandler(taskType string) Handler {
    handlerMu.Lock(); defer handlerMu.Unlock()
    if h, ok := handlers[taskType]; ok { return h }
    return segmentHandler{}
}

// executeJob is now a one-line dispatcher (the lease/fence shell in loop.go is unchanged).
func executeJob(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
    return lookupHandler(job.TaskType).Run(ctx, s, job, epoch, workerID)
}
```

The segment loop body **moves verbatim** into the default handler; only the *loop* relocates —
the package vars (`StepTypeSegment = "segment"`, `segmentMinMs`/`segmentMaxMs`, `defaultSegments`)
the chaos test shrinks stay as `execute.go` package vars:

```go
type segmentHandler struct{}

func (segmentHandler) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
    segments := decodeSegmentCount(job.Payload)      // moved here; reads {"segments":N}, default 5
    start, err := s.LastCompletedStep(ctx, job.ID)
    if err != nil { return err }
    slog.Info("executing job", "job_id", job.ID, "task_type", job.TaskType,
        "segments", segments, "resume_from", start, "remaining", segments-start)
    for i := start + 1; i <= segments; i++ {
        if err := ctx.Err(); err != nil { return err }
        out, durMs, err := runSegment(ctx, job, i)  // moved here; unchanged bounded-sleep body
        if err != nil { return err }
        if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
            JobID: job.ID, StepNumber: i, StepType: StepTypeSegment,
            Output: out, DurationMs: durMs, WorkerID: workerID, // NEW: per-step attribution
        }); err != nil { return err }                       // ErrFenced propagates as-is
    }
    return nil
}
```

`runSegment`, `decodeSegmentCount` become unexported methods/freestanding funcs used only by
`segmentHandler`. **`worker.Run` is unchanged.** The `workerID` is threaded **only** through the
two internal seams the *normal* tests touch (not the chaos test):

```go
// internal/worker/loop.go  — one trailing param each
func executeWithLease(ctx context.Context, s store.JobStore, job *store.Job, lease time.Duration, workerID string) error
    // body unchanged except: executeJob(jobCtx, s, job, job.LeaseEpoch, workerID)

// runOneJob already has workerID; its one call becomes:
//   executeWithLease(ctx, s, job, lease, workerID)
```

**Required mechanical test edits (explicit, 3 sites):**
- `internal/worker/loop_test.go` — the two `executeWithLease(ctx, s, a, lease)` call sites get `, ""`.
- `internal/worker/execute_test.go:99` — `executeJob(ctx, s, b, b.LeaseEpoch)` becomes `executeJob(ctx, s, b, b.LeaseEpoch, "")`.
- `internal/worker/chaos_test.go` — **no edit** (calls the unchanged 5-arg `worker.Run`).

**Acceptance:** `go test -race ./internal/store ./internal/worker ./internal/api` green with *only* the three documented call-site edits; `chaos_test.go` is byte-identical; a `segments` job completes as before and its trace rows now carry `worker_id`.

---

## Phase 1 — The `LLMBackend` abstraction (Ollama, Groq, Fake) + a retrying client *— the seam you can swap without touching orchestration*

> Two backends prove the abstraction is real, not a hardcoded call. Ollama shows you can run
> inference yourself; Groq gives fast, reliable live demos. The orchestration loop never
> asks *which* one is running — it speaks `Complete(ctx, CompleteRequest)` and parses the
> JSON the model returns. **No native tool-calling API is used**: the model returns a strict
> JSON `Decision` we parse (Phase 3). That is what makes swapping Ollama↔Groq cost **zero**
> changes to the agent loop — the backends differ only in HTTP envelope.
>
> **Scope guardrail:** the retrying HTTP client is *resilience* (retry transient 429/5xx/network,
> respect `Retry-After`, bound to a few tries) — it is **not** rate limiting. It records `Usage`
> but debits nothing, holds no global token budget, enforces no QPS. That deliberate boundary
> keeps **U9 (cost-aware token-bucket, Week 5)** from being secretly started here.

### Task 1.1 — `LLMBackend` interface + shared retry client + `NewFromEnv`
**File:** `internal/llm/llm.go`

```go
package llm

type Message struct{ Role, Content string }                  // system | user | assistant  (no "tool" role)

type CompleteRequest struct {
    Messages []Message
    JSON     bool // force JSON output (Ollama format:"json"; Groq response_format json_object)
}

// Usage is the U9 SEED: recorded on every call, debited by nothing in Week 4.
// In Week 5 a token-bucket limiter consumes (PromptTokens+CompletionTokens) against the
// provider's real free-tier budget.
type Usage struct{ PromptTokens, CompletionTokens int }

type CompleteResponse struct {
    Content      string  // the raw assistant message text
    Usage        Usage
    FinishReason string  // "stop" | "length" | ...
}

type LLMBackend interface {
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
    Name() string  // "ollama" | "groq" | "fake"
}
```

`NewFromEnv() (LLMBackend, error)` selects on `LLM_BACKEND` (`ollama`|`groq`; default `ollama`),
reads the `OLLAMA_*`/`GROQ_*` knobs, and returns a backend sharing one tuned `http.Transport`
(`MaxIdleConnsPerHost` raised; `http.Client{Timeout: LLM_TIMEOUT}`). Backoff/retry lives here too:

```go
// retryTransient runs do() with bounded retries on transient HTTP errors (429, 5xx, network).
// NEVER on 4xx (bad request). Respects ctx (backs off via select{case <-t: case <-ctx.Done()}),
// honors Retry-After for Groq 429 (bounded). Backoff shape mirrors postgres.go's computeBackoff
// (exp + jitter + cap) but LLM-tuned: base 1s, cap 30s, +0.4× jitter, max LLM_MAX_RETRIES.
// This is resilience, NOT a token-bucket — Usage is recorded, never debited.
func retryTransient(ctx context.Context, maxRetries int, do func() (CompleteResponse, error)) (CompleteResponse, error)
```

**Acceptance:** `internal/llm/llm_test.go` (Phase 4) drives `OllamaBackend`/`GroqBackend` against `httptest.NewServer` and asserts the right envelope + retry-on-429-with-`Retry-After` + ctx-cancel-aborts-before-any-commit; `fake.go`'s `FakeBackend` compiles into the interface.

### Task 1.2 — `OllamaBackend` (`ollama.go`)
**File:** `internal/llm/ollama.go`

`POST {OLLAMA_HOST}/api/chat` (default `http://localhost:11434`), body
`{"model":OLLAMA_MODEL,"messages":[...],"stream":false,"format":"json" if req.JSON}`;
parse `message.content`, usage from `prompt_eval_count`/`eval_count`. Ollama's `format:"json"`
is what forces valid JSON output on a model without a native JSON mode — that is why the
backend-agnostic protocol does not depend on Groq-specific tool calling.

**Acceptance:** `Complete` against an `httptest` Ollama-shape server returns `Content`/`Usage`; a 500 once then 200 yields one retry then success; a non-2xx 4xx yields no retry and a wrapped error.

### Task 1.3 — `GroqBackend` (`groq.go`)
**File:** `internal/llm/groq.go`

`POST {GROQ_BASE_URL}/openai/v1/chat/completions` (default `https://api.groq.com/openai/v1`),
header `Authorization: Bearer $GROQ_API_KEY`, body
`{"model":GROQ_MODEL,"messages":[...],"response_format":{"type":"json_object"} if req.JSON}`;
parse `choices[0].message.content` + `usage.prompt_tokens`/`completion_tokens`; on `429` honor
`Retry-After` (seconds) within `retryTransient`. Missing `GROQ_API_KEY` → `NewFromEnv` returns a
clear error only when `LLM_BACKEND=groq` is selected (Ollama is the zero-config default).

**Acceptance:** envelope test asserts the `Authorization` header and `response_format` are sent; `Retry-After` is honored (the scheduler waits roughly that long, not `computeBackoff`, before the next try).

### Task 1.4 — `FakeBackend` (`fake.go`) *— the U10 seed*
**File:** `internal/llm/fake.go`

```go
// FakeBackend returns a scripted sequence of Decisions (each encoded as an assistant message)
// and exposes CallCount(). NO time, NO network — it is the seed of the deterministic-time
// simulation that U10 grows into a full FakeClock in Week 7.
type FakeBackend struct {
    mu        sync.Mutex
    script    []string   // each is a JSON Decision (pre-serialized), returned in order
    idx       int
    callCount int
}
func NewFakeBackend(script ...string) *FakeBackend
func (f *FakeBackend) Reset(); func (f *FakeBackend) CallCount() int
func (f *FakeBackend) Complete(ctx context.Context, _ CompleteRequest) (CompleteResponse, error)
func (f *FakeBackend) Name() string  // "fake"
```

- **Why a `Messages`-based interface, not the implementation-plan's `Complete(ctx, prompt) (string,error)`:** the agent resume loop reconstructs a multi-turn conversation (`system, user, assistant(plan), user(Observation), …`). A single-prompt signature forces lossy manual concatenation and drops role semantics; `[]Message` keeps it faithful. This is the one place where the as-built surface (multi-turn) supersedes the original plan's sketch.
- **U9 seed, explicitly not U9:** `Usage` is captured here. The Week-5 limiter will consume it; Week 4 records and discards.

**Acceptance:** `FakeBackend` advances through `script` and exposes `CallCount` so Phase 3's resume test can assert "the LLM was NOT re-called after a mid-iteration crash."

---

## Phase 2 — Tools & a sandboxed `exec` *— the act in plan-act-observe*

> A tool is a fenced step's side effect behind a typed interface. We pick tools that are
> **idempotent** by construction (`search_kb` is a pure read; `run_tests` writes only to a
> fresh per-job temp dir) so that the residual at-least-once-under-hard-kill convergence
> story holds: if a worker dies mid-subprocess and a reclaimer re-runs `run_tests` with the
> same args, it writes a fresh temp dir and gets an identical result. The tool that **fails
> a test** is not a tool that *errors* — `run_tests` *always* returns a `RunTestsResult`
> (pass or fail) as JSON; only ctx cancellation / syscall-level infra surface as the
> `error` return, and the loop treats `ctx.Err()` as "abandon, do not commit."
>
> **Sandbox threat model (stated honestly, not oversold):** `os/exec` with a process-group,
> a `WithTimeout`, a job-scoped temp dir + `RemoveAll`, and a minimal env (PATH only) defends
> time bounds, FS scope, process-group kill-on-cancel, and resource-ish limits. It does **not**
> defend network egress, the full syscall surface, or hard-`kill -9` orphanage. That is
> acceptable for the demo's threat model (user's own VM, ephemeral LLM-generated CP
> solutions, LLM-generated args). Stretch hardening (seccomp / bwrap / firejail) lands in
> Phase 6. We say all of this in the README and in comments — defensible, not hand-waved.

### Task 2.1 — `Tool` interface + `Registry`
**File:** `internal/tools/tools.go`

```go
package tools

// Tool is a single, named, sandboxed capability an agent may invoke. The agent decides
// WHEN to call it (a "plan" step); the loop dispatches and checkpoints the result
// (a "tool_call" step). finish is NOT a Tool — it's a loop-level action (see Phase 3).
type Tool interface {
    Name() string            // "search_kb", "run_tests"
    Description() string     // prose for the system-prompt catalog
    ArgsSchema() string      // JSON-schema-ish string for the prompt
    Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

type Registry struct{ tools map[string]Tool }

func NewRegistry() *Registry
func (r *Registry) Register(t Tool)
func (r *Registry) Get(name string) (Tool, bool)
func (r *Registry) Catalog() []Tool   // for buildSystemPrompt
```

**Acceptance:** `NewRegistry().Register(t); _, ok := r.Get("x")` round-trips; unknown name → `ok==false`.

### Task 2.2 — `search_kb`: read-only, deterministic knowledge base
**Files:** `internal/tools/search_kb.go`, `internal/tools/kb/*.md` (2–4 CP-pattern notes)

```go
// SearchKB is an embedded, read-only library of competitive-programming patterns.
// Args: {"query":"…"}. Returns a JSON array of {pattern, snippet, relevance} ranked by
// token-overlap with the query. Embeds kb/*.md via go:embed (stdlib, zero deps, no vectors).
// Deterministic → idempotent → exactly-once under both kill models (re-runs give identical
// results; no side effects).
type SearchKB struct{ kb []kbDoc }   // loaded from the embed.FS in NewSearchKB
func NewSearchKB() *SearchKB          // `//go:embed kb/*.md`
```

`kb/*.md` holds 2–4 hand-written notes (e.g. prefix-sum, two-pointers, sliding window). The agent
searches them before writing a solution. **Why keyword-overlap not vectors:** free, deterministic,
no GPU/embedding service, and testable with neither DB nor network.

**Acceptance:** `tools_test.go`'s `TestSearchKB` asserts determinism (same query → same ordering) and that the result is valid JSON the agent can ingest.

### Task 2.3 — `run_tests`: sandboxed exec (Python first, process-group, timeout, cleanup)
**Files:** `internal/tools/run_tests.go`, `run_tests_unix.go` (`//go:build !windows`), `run_tests_windows.go` (`//go:build windows`)

```go
// args: {"language":"python","code":"…","cases":[{"name":"…","stdin":"…","expected":"…"}]}
// Returns RunTestsResult ALWAYS (pass or fail). A failing case is normal data the agent
// iterates on. Only ctx cancellation / syscall-level infra errors → the error return.
type RunTestsResult struct {
    Passed     bool         `json:"passed"`
    PerCase    []CaseResult `json:"per_case"`
    Stdout     string       `json:"stdout"`
    Stderr     string       `json:"stderr"`
    ExitCode   int          `json:"exit_code"`
    DurationMs int          `json:"duration_ms"`
}
type CaseResult struct{ Name string `json:"name"`; Passed bool `json:"passed"`; Got string `json:"got"`; Expected string `json:"expected"` }
```

Portable logic in `run_tests.go`: `os.MkdirTemp("", "forge-run-*")`; write `solution.py` from
`args.code`; per case run `exec.CommandContext(ctx, RUN_TESTS_PYTHON, scriptPath)` with
`cmd.Stdin=c.Stdin`, capture stdout/stderr, compare trimmed stdout to `c.Expected`; `defer os.RemoveAll(dir)`. `setProcessGroup(cmd)` + a cancel hook make the subprocess abort on ctx.

The process-group split is **required, not cosmetic**: `syscall.SysProcAttr{Setpgid:true}` is
Unix-only and does not compile on Windows (the dev box), and a CP solution may spawn children we
must reap on cancel. So:

```go
// run_tests_unix.go  //go:build !windows
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
// cmd.Cancel kills the whole group on ctx cancel (graceful/chaos-model kill).
func killGroup(cmd *exec.Cmd) error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
```
```go
// run_tests_windows.go  //go:build windows
func setProcessGroup(cmd *exec.Cmd) {}      // Windows: leave defaults; expansion below.
// cmd.Cancel defaults to TerminateProcess (already whole-process here); killGroup is a no-op.
func killGroup(cmd *exec.Cmd) error { return nil }
```
`run_tests.go` wires `cmd.Cancel = func() error { return killGroup(cmd) }` (Unix) so a `withTimeout`
or leaked ctx cancels the *group*, not just the direct child. Windows keeps the default.

- **Exactly-once under the chaos model (ctx-cancel):** `cmd.Cancel`/timeout kills the group
  *before* `run_tests` returns → no `tool_call` step commits → the reclaimer re-runs with
  identical args → fresh temp dir → identical `RunTestsResult` → exactly-once commit, exactly-once
  *completed* execution.
- **Under hard `kill -9` of the worker process:** a running `python3` may orphan (reparented to
  init). The reclaimer re-runs (the dead attempt never committed) → at-most-once commit,
  **at-least-once execution converging** to the identical result. We document this gap; the
  chaos test's in-process cancel model doesn't expose it, and the live demo uses the lease-expiry
  reclaim path (a graceful-ish kill via container stop, not process SIGKILL).

**Acceptance:** `tools_test.go`'s `TestRunTests_*` — passing/failing cases return the right
`RunTestsResult`; `TestRunTests_Timeout` (ctx past the run's own `RUN_TESTS_TIMEOUT`) kills the
group and the call returns an error; `TestRunTests_Cleanup` asserts the temp dir is gone. Skips
Windows-safely if `RUN_TESTS_PYTHON` is unset (no `python3`).

### Task 2.4 — `finish` is a loop-level action, **not** a registered `Tool`
**File:** none (documented in `internal/agent`, Phase 3). The agent emits `action:"finish"`
with `{"solution","summary"}` and `done:true`. The loop intercepts it *before* dispatch to the
registry, so the model cannot `Tool.Call("finish")`; the system prompt merely lists it as a valid
action. This keeps the "the plan row records the finish-decision" invariant clean and avoids a
redundant `tool_call` row.

**Acceptance:** `finish` never reaches `Registry.Get`; the agent's final `plan` row carries the
finish-decision, and `runOneJob` then calls `CompleteJob`.

---

## Phase 3 — The agent loop *— plan→tool-call→observe, crash-recoverable, exactly-once* (the week's core)

> This is the design decision the whole week exists to resolve: a plan→act→observe **agent** loop
> is *dynamic* (the LLM decides how many steps), but Week-3 checkpoints are keyed by a monotonically
> increasing `step_number` and resume at `LastCompletedStep + 1`. The resolution is a **two-row-per-
> iteration** protocol with a **mid-iteration resume** that never re-spends the LLM:
>
> - Every iteration writes **two** fenced steps: **`plan`** (step `2k-1`) = the LLM decision, then
>   **`tool_call`** (step `2k`) = the tool result. The expensive, nondeterministic LLM decision is
>   checkpointed **before** any side-effecting tool runs.
> - On reclaim, `reconstructHistory` rebuilds the conversation from the committed `plan`/`tool_call`
>   rows and detects a **mid-iteration** state (last committed step is a `plan` whose `tool_call`
>   never committed). It then **reuses that checkpointed decision, skips the LLM call, and runs
>   the tool directly**. So a crash in the window `[plan-committed, tool-committed)` re-runs only
>   the (idempotent) tool — never the LLM.
>
> One-row-per-iteration (fusing decision+result) was rejected: it would, on a crash between the
> LLM call and the tool result, force re-running **both** the LLM (tokens re-spent) and the tool.
> The cost of two-row is 2× rows (a ~5–12-iteration demo = 10–24 rows — trivial). This is the
> project's "durable record of the expensive decision = recovery is resumption, not restart" thesis,
> applied to the agent — and the mid-iteration-reuse is the demo's most compelling beat.
>
> **Exactly-once, honestly:** *exactly-once step COMMIT* and *exactly-once job completion* hold
> (the `RecordStep` boundary). The "never re-spend the LLM" guarantee holds **when the plan already
> committed** before the kill — if the kill lands mid-LLM-call, the plan never committed and the LLM
> is genuinely re-called (best-effort token savings, never a step-OO violation). Residual
> at-least-once *execution* is only under literal `kill -9` (subprocess orphan), mitigated by
> idempotent tools whose results converge and by the orphan's result never committing. The chaos
> test's **ctx-cancel** model gives the agent full exactly-once (commit + execution), because an
> aborted call never reaches `RecordStep`.

### Task 3.1 — The `Agent` value + the `Run` loop
**Files:** `internal/agent/agent.go`, `internal/agent/decision.go`, `internal/agent/history.go`

```go
// internal/agent/agent.go
package agent

type Config struct{ MaxSteps int }   // AGENT_MAX_STEPS (default unknown-action-iteration cap);

type Agent struct {
    llm   llm.LLMBackend   // Phase 1 (baked in cmd/worker, not in worker pkg — no import cycle)
    tools tools.Registry   // Phase 2
    cfg   Config
}
func New(cfg Config, be llm.LLMBackend, reg tools.Registry) *Agent

// Run implements worker.Handler. It resumes from ListSteps, reconstructs history, and drives the
// plan→tool→observe loop, checkpointing each plan/tool_call via store.RecordStep (fenced by epoch).
// Returns: nil on finish; ctx.Err() on shutdown (ABANDON for reclaim — NO FailJob, no commit);
// store.ErrFenced on depose (propagated, no commit); a non-nil error on infra/parse/max-steps
// (→ runOneJob calls FailJob → requeue/dead-letter per max_attempts).
func (a *Agent) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error
```

The loop (pseudocode — the exact fence/commit discipline matters):

```go
func (a *Agent) Run(ctx, s, job, epoch, workerID) error {
    sysPrompt := buildSystemPrompt(a.tools)               // catalog + Decision schema + finish
    taskMsg, err := buildUserTask(job.Payload)            // {"problem":...,"language":...}
    if err != nil { return err }

    steps, _ := s.ListSteps(ctx, job.ID)
    msgs, pending, alreadyDone := reconstructHistory(sysPrompt, taskMsg, steps)
    if alreadyDone { return nil }                          // crashed between finish-plan & CompleteJob
    nextStep := lastCompletedNumber(steps) + 1

    var iterationsSinceCheckpoint int
    for iterationsSinceCheckpoint = 0; iterationsSinceCheckpoint < a.cfg.MaxSteps; iterationsSinceCheckpoint++ {
        if err := ctx.Err(); err != nil { return err }     // shutdown → abandon, no commit

        var dec Decision
        if pending != nil {                                // MID-ITERATION RESUME: reuse committed plan
            dec, pending = *pending, nil                   // skip the LLM call; nextStep already points at the tool_call slot
        } else {
            resp, err := a.llm.Complete(ctx, CompleteRequest{Messages: msgs, JSON: true})
            if errors.Is(err, ctx.Err()) { return err }    // shutdown/fence abandon
            if err != nil { return err }                   // infra error → FailJob (requeue/DLQ)
            dec, err = parseDecision(resp.Content)
            if err != nil { dec, err = a.retryOnce(ctx, msgs, sysPrompt) }   // nudge + one retry
            if err != nil { return fmt.Errorf("agent: unparseable LLM decision: %w", err) } // → FailJob
            stepOut, _ := json.Marshal(dec)
            if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
                StepNumber: nextStep, StepType: "plan", Output: stepOut, WorkerID: workerID,
            }); err != nil { return err }                  // ErrFenced propagates
            msgs = append(msgs, Message{Role: "assistant", Content: string(stepOut)})
            nextStep++
        }

        if dec.Done { // action == "finish": the plan row already recorded the finish-decision
            return nil                                     // runOneJob → CompleteJob
        }

        tool, ok := a.tools.Get(dec.Action)
        var out json.RawMessage
        if !ok {
            out, _ = json.Marshal(map[string]string{"error": "unknown tool: " + dec.Action})
        } else {
            res, err := tool.Call(ctx, dec.Args)
            if errors.Is(err, ctx.Err()) { return err }    // shutdown/fence → abandon, NO commit
            if err != nil { out, _ = json.Marshal(map[string]string{"error": err.Error()}) }
            else { out = res }
        }
        in, _ := json.Marshal(map[string]any{"action": dec.Action, "args": dec.Args})
        if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
            StepNumber: nextStep, StepType: "tool_call", Input: in, Output: out, WorkerID: workerID,
        }); err != nil { return err }                      // ErrFenced propagates
        msgs = append(msgs, Message{Role: "user", Content: "Observation: " + string(out)})
        nextStep++
    }
    return fmt.Errorf("agent exceeded max steps")          // → FailJob (mitigates the no-attempt-guard reclaim loop)
}
```

### Task 3.2 — `reconstructHistory`: backend-agnostic conversation replay
**File:** `internal/agent/history.go`

```go
func reconstructHistory(systemPrompt, task string, steps []store.JobStep) (msgs []llm.Message, pending *Decision, alreadyDone bool)
```

Mapping (no native "tool" role — the model sees plain `assistant`/`user` turns):
- Lead with `{system, systemPrompt}`, `{user, task}`.
- For each step in `step_number` order: `plan` → `{assistant, string(step.Output)}`; `tool_call` → `{user, "Observation: " + string(step.Output)}`.
- After replay: if the last committed step is a `plan`, parse its `Decision`. If `Done` → `alreadyDone=true`. Else → `pending=&decision` (mid-iteration resume). Else `pending=nil`.

**Acceptance:** given steps `[{2k-1:plan, dec j}, {2k:tool_call}]…`, the rebuilt `msgs` is a legal multi-turn transcript; a trailing lone `plan` yields `pending`, never a duplicate assistant turn.

### Task 3.3 — `Decision` schema + `parseDecision` + `extractJSON`
**File:** `internal/agent/decision.go`

```json
{ "thought": "<brief chain-of-thought>",
  "action":  "search_kb | run_tests | finish",
  "args":    { "<action-specific>" },
  "done":    false }
```
`finish.args = {"solution":"<final answer>","summary":"…"}` with `done:true`. `search_kb.args={"query"}`,
`run_tests.args={"language","code","cases":[{...}]}`. `buildSystemPrompt(reg)` writes the catalog
(`Name/Description/ArgsSchema` of each registered tool), the `finish` action, the exact schema, and:
*"Reply with ONLY a JSON object — no markdown fences, no prose. Tool results arrive as user
messages prefixed `Observation:`."* — the swap-Ollama↔Groq-zero-change abstraction point.

`parseDecision(content)`: strip a markdown fence if present; `json.Unmarshal`; on failure, slice
from the first `{` to the last `}` and retry; else error. `retryOnce` appends one
`{user,"reply with ONLY JSON"}` nudge and re-calls the LLM once — a second failure → `FailJob`.

**Acceptance:** parses fenced JSON, raw JSON, and "prose + {...} + prose"; rejects garbage with an error. Tests cover the nudge-then-fail and nudge-then-succeed paths.

### Task 3.4 — Interaction with the lease (NO new leasing code)
The already-wired U2 `extenderLoop` renews `lease/3` from a *separate* goroutine — it is not
blocked by a 60s `LLM_TIMEOUT` LLM call. ctx flows into `req.WithContext(ctx)` (HTTP abort) and
`exec.CommandContext` (group kill), so a shutdown/fence aborts both in-flight calls before any
`RecordStep`. **No lease code is written in Week 4** — the agent is just a `Handler` body.

### Task 3.5 — `AGENT_MAX_STEPS` vs the `ClaimJob`-no-`max_attempts`-guard gap
`ClaimJob` increments `attempt_count` with no guard (a poison job reclaim-loops forever). The agent
bounds this: exceeding `MaxSteps` → `FailJob` → requeue (`attempt < max_attempts`) or dead-letter
(`≥ max_attempts`). Crucially, `reconstructHistory` reuses **already-committed** plan/observation
rows on resume, so earlier LLM calls are *not* re-spent — `MaxSteps` bounds iterations *since the
last checkpoint*, not cumulative. A poison agent job burns ≤ `MaxSteps × max_attempts` (default
`12×3 = 36`) LLM iterations, then dead-letters. Segment jobs were always finite by `segments`.

**Acceptance (Phase 3 = Phase 4's tests):** `internal/agent/agent_test.go` — `TestAgentLoop_FinishCompletes`,
**`TestAgentLoop_ResumeMidIterationAfterDepose`** (THE thesis test for the agent — see Phase 4),
`TestAgentLoop_MaxStepsFailJob`, `TestAgentLoop_BadJSONRetriesOnceThenFails`, `TestAgentLoop_RealStoreSmoke`.

---

## Phase 4 — Tests *— stdlib `testing`, DB-backed, `-race`; the agent's resume thesis test*

> The Week-3 tests already prove the SQL, fencing, and exactly-once scaffold against real
> Postgres and the chaos model. The agent tests split by **altitude**: logic/resume/oracle/max-
> steps/parse tests use a **faithful in-memory fake store** (fast, no DB, no Docker, no real-time
> lease races — they control "cancel mid-iteration" and "resume with a new epoch" precisely), and
> ONE real-Postgres smoke covers the `RecordStep`/`ListSteps` SQL + `worker_id` regression. This
> mirrors the repo's existing split (`store`/`worker` use real Postgres; `api` uses `memStore`;
> `worker` chaos uses its own `chaosStore`). stdlib `testing` only — no testify (the `go.sum`
> testify line is a stale unused hash).

### New test files
- `internal/llm/llm_test.go` — `httptest.NewServer`-driven:
  - Ollama vs Groq request **envelope** (assert `format:"json"` / `response_format.json_object` / `Authorization: Bearer`);
  - **retry on transient**: one `500` then `200` succeeds after exactly one retry; `429` + `Retry-After: 2` honors the header; a `400` does NOT retry (returns wrapped error);
  - **ctx-cancel aborts before any commit**: cancel the request's ctx mid-flight → `Complete` returns `ctx.Err()`, no HTTP body written.
  - `FakeBackend` advances the script and `CallCount` increments.
- `internal/tools/tools_test.go`:
  - `TestSearchKB` — deterministic ordering, valid JSON.
  - `TestRunTests_PassingAndFailing` — Python; returns the right `RunTestsResult` for both outcomes; **skip** if `RUN_TESTS_PYTHON` is unset (Windows-safe).
  - `TestRunTests_Timeout` — ctx past `RUN_TESTS_TIMEOUT` kills the group; call returns an error.
  - `TestRunTests_Cleanup` — temp dir is removed.
  - `TestRegistry` — register/get/catalog.
- `internal/agent/agent_test.go`:
  - a faithful **`fakeStore`** (mutex-guarded, mirrors `chaosStore`'s fenced `RecordStep`/`ListSteps`/`LastCompletedStep`; `RecordStep` honors epoch + `ctx.Err()` and bumps a per-(job,step) commit counter);
  - a **`countingTool`** wrapper that increments **only on a non-cancelled return** (`if ctx.Err()!=nil return at entry and after the inner call`) — mirroring the chaos oracle's commit-boundary semantics, so under ctx-cancel the aborted attempt never increments;
  - a local **`newTestStoreTB`** copy (~15 lines: `DATABASE_URL` with docker-compose fallback, `t.Skipf` on connect-fail, `TRUNCATE job_steps CASCADE; TRUNCATE jobs CASCADE; TRUNCATE workers CASCADE;` before/after) — `newTestStoreTB` lives in package `worker` and is not importable by `agent`, consistent with the existing "each package has its own helper" pattern.
  - `TestAgentLoop_FinishCompletes` — `FakeBackend` script `[search_kb(not done), finish]` + `countingTool` → job completed; trace is `{plan(1), tool_call(2), plan(3,done)}` with correct step numbers.
  - **`TestAgentLoop_ResumeMidIterationAfterDepose`** — THE thesis test for the agent, paralleling the chaos test's role:
    script `[search_kb(not done), finish]`.
    **Run 1** (epoch 1, worker "w-a"): LLM call#1 → `plan`(step1) committed; then **cancel ctx before the tool commits** (the `countingTool`'s `Call` `select`s on `ctx.Done()`). Run returns `ctx.Err()` — **no step2 committed, counter untouched.**
    **Simulate reclaim:** expire the lease in `fakeStore`, bump the epoch (epoch 2).
    **Run 2** (worker "w-b"): `reconstructHistory` sees step1 `plan`, no step2 → `pendingDecision = dec1`, **no LLM call**; dispatch `search_kb` → `countingTool` counter[argsKey]==1 → `tool_call`(step2) committed; iter2 LLM call#2 → `finish` → `plan`(step3, done) → return nil.
    **Assert:** `FakeBackend.CallCount()==2` (decision1 NOT re-called); `countingTool` counter[argsKey]==1; trace rows `{plan(1), tool_call(2), plan(3,done)}` each committed exactly once; final status `completed`.
  - `TestAgentLoop_MaxStepsFailJob` — `FakeBackend` never returns `done` → after `MaxSteps` iterations, `Run` returns a non-nil error (the caller `FailJob`s it).
  - `TestAgentLoop_BadJSONRetriesOnceThenFails` — non-JSON once → nudge → valid on retry (success); non-JSON twice → nudge → still bad → `Run` returns a parse error.
  - `TestAgentLoop_RealStoreSmoke` — via the local `newTestStoreTB`, a finish-on-first-call `FakeBackend` → completes against **real Postgres**; assert the trace incl. a **non-empty `worker_id`** (the `worker_id` regression catch).

### Chaos test stays byte-identical
`internal/worker/chaos_test.go` is **not edited** — it calls the unchanged 5-arg `worker.Run`, which
internally threads `workerID` to the segment handler (a new capability that changes no assertion).
The `worker_id` migration does not touch the in-memory `chaosStore`.

### Stretch (optional, Phase 6)

  `internal/agent/agent_chaos_test.go` — a U7-style chaos/invariant test for the *agent* loop
  (`FakeBackend` + `countingTool`, seeded killer, assert liveness + exactly-once-commit + `-race`).
  **Flagged as the U10 deterministic-time predecessor:** it still uses real sleeps/timeouts;
  the **full `FakeClock`** arrives Week 7. Implement only if Phase 5 is already green and time remains.

**Acceptance (Phase 4):** `go test -race ./internal/llm ./internal/tools ./internal/agent` green on **Windows** (tools skip when `python3`/`RUN_TESTS_PYTHON` absent) and Linux; `go test -race -count=5 ./internal/worker/...` still green with `chaos_test.go` untouched.

---

## Phase 5 — Wiring, env knobs, images, compose *— make the binary register the agent*

> Phases 1–3 built isolated packages; Phase 5 connects them in `cmd/worker/main.go`. The
> worker package imports **none** of `llm`/`tools`/`agent` — the binary bakes deps into the
> `agent.Agent` and `worker.RegisterHandler`s it as a `worker.Handler`. The lease extender,
> the poll loop, and `worker.Run` are unchanged. This is where "swap the backend without
> touching orchestration" becomes a real, runnable claim: set `LLM_BACKEND=groq` and the whole
> agent moves to Groq with zero code changes.

### Task 5.1 — `cmd/worker/main.go` constructs the agent and registers it
**File:** `cmd/worker/main.go`

After the existing `concurrency` parse (line ~70), before `signal.NotifyContext`, add the
deps + handler registration (mirroring the existing `os.Getenv` + warn-on-bad-value idiom):

```go
import (
    // add to the existing block:
    "forge/internal/agent"
    "forge/internal/llm"
    "forge/internal/tools"
)

    // --- Week 4: build the agent backend + tools, register as a worker.Handler.
    be, err := llm.NewFromEnv()
    if err != nil {
        // Only fatal if misconfigured *and* a backend was reachable; Ollama as the
        // zero-config default means a dev box without GROQ_API_KEY still boots.
        slog.Warn("LLM backend misconfigured; cp_solve jobs will FailJob", "error", err)
    } else {
        slog.Info("LLM backend selected", "backend", be.Name())
    }

    reg := tools.NewRegistry()
    reg.Register(tools.NewSearchKB())
    reg.Register(tools.NewRunTests(
        envDuration("RUN_TESTS_TIMEOUT", 5*time.Second),  // default 5s
        envStrDefault("RUN_TESTS_PYTHON", "python3"),
    ))

    maxSteps := envIntDefault("AGENT_MAX_STEPS", 12)
    worker.RegisterHandler("cp_solve", agent.New(agent.Config{MaxSteps: maxSteps}, be, reg))
    slog.Info("registered agent handler", "task_type", "cp_solve", "max_steps", maxSteps)

    // --- existing code below is unchanged:
    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()
    if err := worker.Run(ctx, pgStore, workerID, lease, concurrency); err != nil && err != context.Canceled {
        log.Fatalf("worker stopped: %v", err)
    }
```

`envDuration`/`envIntDefault`/`envStrDefault` are small local helpers in `main.go` (same style as
the existing inline `WORKER_LEASE`/`WORKER_CONCURRENCY` parse; invalid → warn + default, never fatal).
**`worker.Run`'s signature is unchanged.**

### Task 5.2 — Exact env knobs (the complete Week-4 set)
Every new knob is read by `llm.NewFromEnv` (Phase 1) or `cmd/worker/main.go` (Task 5.1). Format matches the existing `WORKER_*` knobs.

| Env var | Default | Required? | Read where | Notes |
|---|---|---|---|---|
| `LLM_BACKEND` | `ollama` | no | `llm.NewFromEnv` | `ollama` \| `groq`. Ollama is the zero-config default (local). |
| `OLLAMA_HOST` | `http://localhost:11434` | no | `llm.NewFromEnv` | Ollama `/api/chat` base URL. |
| `OLLAMA_MODEL` | `llama3.1` (or installed) | no | `llm.NewFromEnv` | Pick a model the host actually has (`ollama list`). |
| `GROQ_API_KEY` | _(empty)_ | **only if `LLM_BACKEND=groq`** | `llm.NewFromEnv` | Missing + backend=groq → `NewFromEnv` error → handler warns, `cp_solve` jobs `FailJob`. |
| `GROQ_BASE_URL` | `https://api.groq.com/openai/v1` | no | `llm.NewFromEnv` | |
| `GROQ_MODEL` | `llama-3.1-8b-instant` | no | `llm.NewFromEnv` | Fast free-tier Groq model. |
| `LLM_TIMEOUT` | `60s` | no | `llm.NewFromEnv` | Per-HTTP-retry-call timeout. Covered by lease extender under any `WORKER_LEASE`. |
| `LLM_MAX_RETRIES` | `3` | no | `llm.NewFromEnv` | Bounded transient retry (resilience, NOT rate-limiting). |
| `AGENT_MAX_STEPS` | `12` | no | `cmd/worker/main.go` | Per-resume iteration cap → `FailJob` (mitigates the no-attempt-guard reclaim loop). |
| `RUN_TESTS_TIMEOUT` | `5s` | no | `cmd/worker/main.go` | Per-subprocess-case timeout; ctx kills the group. |
| `RUN_TESTS_PYTHON` | `python3` | no | `cmd/worker/main.go` | Interpreter path (`python` on Windows). Empty/unfound → `run_tests` callers `FailJob`. |

Existing knobs (unchanged): `DATABASE_URL` (required), `WORKER_ID` (default `hostname-8hex`), `WORKER_LEASE` (default `2m`), `WORKER_CONCURRENCY` (default `1`).

**Acceptance:** `cmd/worker/main.go` compiles with the additions; with no LLM env at all (Ollama default, even if the daemon is absent) the worker **still boots and drains `segments` jobs** — only `cp_solve` jobs `FailJob` (the agent's LLM call errors → non-nil `Run` error → `FailJob` → requeue/dead-letter). The system never fails to start because of an LLM config gap.

### Task 5.3 — `Dockerfile.worker` adds `python3`
**File:** `Dockerfile.worker` (runtime stage only — the build stage is unchanged)

```dockerfile
# ---- Runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache python3            # Week 4: run_tests executes LLM-generated solutions
WORKDIR /app
COPY --from=build /out/worker /app/worker
ENTRYPOINT ["/app/worker"]
```

- No `g++`/`go` unless the `run_tests` multi-language stretch (Phase 6) lands. `python3` is ~50MB on alpine and keeps the image lean. CGO stays disabled (`GOOS=linux CGO_ENABLED=0` build unchanged).
- **Verify Python runs as `python3` on alpine**, not just `python`: the sandboxed exec calls `RUN_TESTS_PYTHON` (default `python3`); add `python3` to PATH. (Stretch: symlink `python` → `python3`.)

**Acceptance:** `docker compose build worker` succeeds; inside the container `python3 --version` prints; a `run_tests` against a trivial two-case job returns `passed:true`.

### Task 5.4 — `docker-compose.yml` + `.env.example` pass the new env
**Files:** `docker-compose.yml` (every `worker-N` service `environment:`), `.env.example`

Each of the four worker services (`worker-1..worker-4`, already explicit, not `deploy.replicas`)
gains the Week-4 knobs as compose substitutions so `.env`/shell override cleanly:

```yaml
  worker-1:
    # ... existing DATABASE_URL, WORKER_ID: worker-1, WORKER_LEASE, WORKER_CONCURRENCY ...
    environment:
      - DATABASE_URL=${DATABASE_URL}
      - WORKER_ID=worker-1
      - WORKER_LEASE=${WORKER_LEASE:-2m}
      - WORKER_CONCURRENCY=${WORKER_CONCURRENCY:-1}
      - LLM_BACKEND=${LLM_BACKEND:-ollama}
      - OLLAMA_HOST=${OLLAMA_HOST:-http://host.docker.internal:11434}  # host Ollama from a container
      - OLLAMA_MODEL=${OLLAMA_MODEL:-llama3.1}
      - GROQ_API_KEY=${GROQ_API_KEY:-}
      - GROQ_BASE_URL=${GROQ_BASE_URL:-https://api.groq.com/openai/v1}
      - GROQ_MODEL=${GROQ_MODEL:-llama-3.1-8b-instant}
      - LLM_TIMEOUT=${LLM_TIMEOUT:-60s}
      - LLM_MAX_RETRIES=${LLM_MAX_RETRIES:-3}
      - AGENT_MAX_STEPS=${AGENT_MAX_STEPS:-12}
      - RUN_TESTS_TIMEOUT=${RUN_TESTS_TIMEOUT:-5s}
      - RUN_TESTS_PYTHON=${RUN_TESTS_PYTHON:-python3}
```

`.env.example` documents every new knob with a comment + default (mirroring its existing
`DATABASE_URL`/`DUCKDNS_TOKEN`/`WORKER_ID` lines). The `OLLAMA_HOST` default in compose uses
`host.docker.internal` so a developer's *host* Ollama is reachable from the worker container on
Docker Desktop (the Oracle VM, Phase 7, points this at the VM-local Ollama instead).

**Acceptance:** `docker compose up` brings the stack up with the agent wired; `.env.example` lists all knobs; `cp_solve` jobs reach the agent (and `FailJob` cleanly if no backend is reachable).

---

## Phase 6 — Stretch: agent-loop chaos test, multi-language `run_tests`, sandbox hardening

> Core is done at Phase 5. Stretch items are high-signal but not blocking — implement only if
> Phase 5 is green and time remains. Each is independently shippable.

### Task 6.1 — Agent-loop chaos/invariant test (U7-style, U10 predecessor)
**File:** `internal/agent/agent_chaos_test.go`

A `TestChaosRecoveryKillsExactlyOnce_Agent` mirroring `internal/worker/chaos_test.go` but for the
dynamic agent loop: `FakeBackend` (deterministic decisions) + `countingTool` (commits-only
counter), M jobs, N worker goroutines, a **seeded** in-process ctx-cancel killer (`CHAOS_SEED`).
Assert the three invariants: (1) liveness (every job `completed` or `dead_letter`); (2) safety
(`countingTool` per-(job,step) == 1, no step **committed** twice, per-job steps cover the right
span with no gaps); (3) no panic/data-race under `-race`. Run: `go test -race -count=5 ./internal/agent/...`.
**Flagged as the U10 predecessor:** it uses real sleeps/timeouts; the full `FakeClock` to make it
truly deterministic-time lands in **Week 7**. This is the file `FakeClock` later plugs into.

### Task 6.2 — Multi-language `run_tests` (Go/g++)
**Files:** `internal/tools/run_tests.go` (extend dispatch), `Dockerfile.worker` (add `g++`/`go`)

Extend `run_tests` to dispatch on `args.language`: `python` (core), and `go`/`cpp` (this stretch —
add a compile step, then run the binary one-case-at-a-time, same sandbox loop). `Dockerfile.worker`
runtime stage adds the matching toolchain (`go` is heavy; `g++` lighter). Each language keeps the
per-case timeout + group-kill + temp-dir cleanup.

### Task 6.3 — Sandbox hardening (bwrap/firejail/seccomp)
**File:** `internal/tools/run_tests_unix.go`

Replace the bare process-group exec with a bubblewrap/firejail/seccomp wrapper that drops
network egress and the syscall surface; add a hard-kill reaper for the `kill -9` orphan edge. This
closes the residual "at-least-once-execution under hard kill" and "network egress" gaps the
honest threat-model section names. **The README and the task-2.3 threat-model note must be updated
to say "defends: X, Y, Z" only once this actually deploys** — otherwise the note stays at the
honest fallback level. High signal for a security interview; low priority for the demo.

---
## Phase 7 — The agent crash-recovery demo + live redeploy over HTTPS

> The week's checkpoint is a *demonstration*, not just "tests pass": a real multi-step agent job
> (CP: search patterns → write a solution → run tests → finish), checkpointed, then `kill -9` the
> worker mid-loop, then a *different* worker resumes from the last checkpointed decision and
> completes — with the trace showing `plan`/`tool_call` rows attributed to **two** workers, over
> `https://4orge.duckdns.org`. This generalizes the Week-3 kill-recovery demo from dummy segments
> to a real agent — the strongest artifact the project produces.

### Task 7.1 — `scripts/cp_solve_agent_demo.sh`
**File:** `scripts/cp_solve_agent_demo.sh` (mirrors `scripts/kill_recovery_test.sh` for `cp_solve`)

Outline (the script asserts its own PASS/FAIL, like the Week-3 script):
1. POST a `cp_solve` job (`task_type:"cp_solve"`, payload `{"problem":"<a small CP problem>","language":"python"}`) via `https://4orge.duckdns.org/jobs`; capture `job_id` and the first owner (`claimed_by`).
2. Poll `GET /jobs/{id}/trace` until ≥1 `plan` + ≥1 `tool_call` step are committed (the agent is mid-loop).
3. `kill -9` the owning worker container (`docker compose kill -s SIGKILL worker-N`), then restart it to restore the fleet (as in Week 3).
4. Poll until `status=completed`; assert the final owner (`claimed_by`) is a **different** worker than the first.
5. Pull `GET /jobs/{id}/trace`; assert:
   - all steps `status:completed`, `step_number` contiguous with **no** duplicate (`DISTINCT(step_number) = COUNT`),
   - step_types include `plan` and `tool_call`,
   - per-step `worker_id` contains **two distinct** values (the killed worker's early steps + the reclaimer's resumed steps) — the money shot,
   - the last `plan` row's `Decision.done == true` (the finish row).
6. Print `PASS: kill -9 -> different worker resumed agent from checkpoint, exactly-once, two-worker trace` (exit 0) or the failing assertion (exit 1).

### Task 7.2 — `docs/week4_demo.md`
**File:** `docs/week4_demo.md` (mirrors `docs/week3_demo.md`)

A runbook + a captured real transcript: the "What it proves (U4 + the agent loop together)" list,
the kill→reclaim→resume→finish sequence, the trace with per-step `worker_id` by two workers, and
the honest note that mid-LLM-call kills genuinely re-call the LLM (best-effort savings) while
post-`plan` kills reuse the committed decision (zero LLM re-spend). The demo's money-shot is the
`reconstructHistory` reuse visible in the trace: the reclaimer's first action is a `tool_call`,
**not** a `plan` — proof the LLM decision was recovered, not recomputed.

### Task 7.3 — Re-deploy to the Oracle VM over HTTPS
> **Run this on the Oracle VM** (see memory: SSH `ubuntu@4orge.duckdns.org -i ~/.ssh/forge_vm`, repo `~/forge` on `main`).

```bash
# On the VM, on main, pull + re-apply the new migration + rebuild the images
cd ~/forge && git pull origin main
# Apply migration 000004 (the persisted pgdata volume keeps Weeks 1-3 schema intact)
for f in migrations/*.up.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"; done
docker compose build worker orchestrator         # picks up Dockerfile.worker python3
docker compose up -d                             # rolling restart of the 4 workers + orchestrator
docker compose exec worker-1 sh -c 'python3 --version'   # sanity: python present for run_tests

# Pick a backend. For a reliable live demo use Groq free tier (fast, no local GPU):
#   export GROQ_API_KEY=... ; set LLM_BACKEND=groq in the compose/.env and `docker compose up -d` again.
# Or run a local Ollama on the VM and set OLLAMA_HOST + OLLAMA_MODEL to a model `ollama list` shows.

# Run the Year-4 demo:
bash scripts/cp_solve_agent_demo.sh          # hits https://4orge.duckdns.org; prints PASS
```

The Caddy + DuckDNS HTTPS layer from Weeks 1/3 is untouched — `https://4orge.duckdns.org` keeps
serving `/jobs`, `/jobs/{id}/trace`, `/health`. Re-running the demo after any later change is the
command above.

**Acceptance:** the script prints `PASS ...` (exit 0) over HTTPS; `docs/week4_demo.md` records the real transcript with the two-worker `worker_id` trace.

> ✅ **Done (2026-07-31).** Ran against the live VM: job `78e30238-1846-454c-8f89-935127e2fb4b`,
> killed `worker-1` after 2 steps, `worker-3` reclaimed and completed at 9 steps. Exit 0; full
> transcript and evidence in [`docs/week4_demo.md`](docs/week4_demo.md).

---
## Out-of-scope, with seeds planted now

| Item | Lands in | Status in Week 4 | Seed planted now |
|---|---|---|---|
| **U8** OpenTelemetry tracing | W6 | Deferred — no spans, no `go.opentelemetry.io` dep | Pristine ctx propagation: `jobCtx` flows into the LLM HTTP request (`req.WithContext`) and into `exec.CommandContext`; span points marked in comments (`RecordStep`, `LLM.Complete`, `tool.Call`, `CompleteJob`) so Week-6 wraps them without restructuring. |
| **U9** Cost-aware token-bucket limiter | W5 | Deferred — no limiter, no Redis, no QPS meter | `Usage{PromptTokens, CompletionTokens}` in `CompleteResponse`; the retry client **records nothing global and debits nothing** — the explicit boundary so Week-5's limiter is its own code, not a half-built one. |
| **U10** `FakeClock` + deterministic-time sim | W7 | Deferred — no `Clock` interface | `llm.FakeBackend` (scripted, network/time-free) now; agent tests are sleep-free except a tiny `countingTool` cancel `select`; the Phase-6 chaos test is the deterministic-shape predecessor the `FakeClock` later fills in. |

| **In-core (must finish)** | **In-stretch (nice, not blocking)** | **Deferred w/ seed** |
|---|---|---|
| Dispatch registry + `worker.Handler`; `worker.Run` unchanged; `workerID` threading; `worker_id` migration (Phase 0) | Agent-loop chaos/invariant test (Phase 6.1) | U8 OTel (ctx + span points) |
| `LLMBackend` + Ollama/Groq/Fake + retry client + `NewFromEnv` (Phase 1) | Go/g++ `run_tests` languages (Phase 6.2) | U9 cost-aware limiter (`Usage`) |
| `Tool`/`Registry`; embedded `search_kb`; Python `run_tests` (pgid split, timeout, cleanup); `finish` loop-action (Phase 2) | Sandbox hardening: seccomp/bwrap, hard-kill reaper (Phase 6.3) | U10 deterministic-time sim (`FakeBackend`) |
| Agent loop: two-row resume, `reconstructHistory`, `MaxSteps`, nudge, `extractJSON` (Phase 3) | — | — |
| Agent tests incl. the resume thesis test + real-DB smoke (Phase 4) | — | — |
| `cmd/worker` wiring; env knobs; `Dockerfile.worker` python3; compose/.env; demo script + docs + HTTPS redeploy (Phase 5, 7) | — | — |

---

## Progress Tracking Table

> Order matters: Phase 0 (the dispatch seam + `worker_id`) gates the agent handler, which *is* a
> `worker.Handler`. Phases 1→3 can interleave (Phase 3 needs both 1 and 2). Mark ☑ as you land each.

| # | Task | Upgrade / area | Core/Stretch | Status |
|---|------|----------------|--------------|--------|
| 0.1 | `migrations/000004` `worker_id` + `models.go` + `RecordStep`/`ListSteps` SQL | attribution | core | ☑ |
| 0.2 | `executeJob` → dispatcher; `segmentHandler`; `Handler`/`RegisterHandler`; 3 test call-site edits | dispatch seam | core | ☑ |
| 1.1 | `internal/llm/llm.go`: `LLMBackend` interface, `Message`/`Usage`/`CompleteRequest/Response`, retry client, `NewFromEnv` | LLM abstraction (U9 seed) | core | ☑ |
| 1.2 | `internal/llm/ollama.go` (POST `/api/chat`, `format:"json"`) | backend | core | ☑ |
| 1.3 | `internal/llm/groq.go` (`Authorization`, `response_format`, `Retry-After`) | backend | core | ☑ |
| 1.4 | `internal/llm/fake.go` (`FakeBackend`, `CallCount`) | U10 seed | core | ☑ |
| 2.1 | `internal/tools/tools.go` `Tool`/`Registry` | tools | core | ☑ |
| 2.2 | `internal/tools/search_kb.go` + `kb/*.md` (embedded, deterministic) | tools | core | ☑ |
| 2.3 | `internal/tools/run_tests.go` + `run_tests_{unix,windows}.go` (pgid, timeout, cleanup) | sandbox | core | ☑ |
| 2.4 | `finish` loop-level action (documented in agent) | tools | core | ☑ |
| 3.1 | `internal/agent/agent.go` `Agent.Run` (two-row loop, mid-iteration resume) | agent loop | core | ☑ |
| 3.2 | `internal/agent/history.go` `reconstructHistory` | agent loop | core | ☑ |
| 3.3 | `internal/agent/decision.go` `Decision`/`parseDecision`/`buildSystemPrompt` | agent loop | core | ☑ |
| 3.4 | Lease interaction (NO new code — rely on existing U2 extender) | lease | core | ☑ |
| 3.5 | `AGENT_MAX_STEPS → FailJob` bound on the no-attempt-guard gap | robustness | core | ☑ |
| 4.1 | `internal/llm/llm_test.go` (envelope, retry, ctx-cancel) | tests | core | ☑ |
| 4.2 | `internal/tools/tools_test.go` (search/run/cleanup, Windows-skip) | tests | core | ☑ |
| 4.3 | `internal/agent/agent_test.go` incl. `TestAgentLoop_ResumeMidIterationAfterDepose` | thesis test | core | ☑ |
| 4.4 | `internal/agent/agent_test.go` real-Postgres smoke (`worker_id` regression) | tests | core | ☑ |
| 5.1 | `cmd/worker/main.go` constructs agent + `RegisterHandler("cp_solve")` | wiring | core | ☑ |
| 5.2 | env knobs table implemented + `.env.example` documented | config | core | ☑ |
| 5.3 | `Dockerfile.worker` adds `python3` | images | core | ☑ |
| 5.4 | `docker-compose.yml` passes new env to all 4 workers | compose | core | ☑ |
| 6.1 | `internal/agent/agent_chaos_test.go` (U7-style, U10 predecessor) | chaos test | stretch | ☐ |
| 6.2 | Go/g++ `run_tests` languages + image toolchains | tools | stretch | ☐ |
| 6.3 | Sandbox hardening (seccomp/bwrap, hard-kill reaper) + threat-model note update | security | stretch | ☐ |
| 7.1 | `scripts/cp_solve_agent_demo.sh` (asserts PASS/FAIL) | demo | core | ☑ |
| 7.2 | `docs/week4_demo.md` runbook + transcript | docs | core | ☑ |
| 7.3 | Re-deploy to Oracle VM over HTTPS; demo `PASS` | deploy | core | ☑ |

---

## Week 4 Checkpoint (must be demo-able)

> A real `cp_solve` agent job is running on worker A: search pattern → write a solution → run
> tests → finish. `kill -9` A **after** it has committed at least one `plan` step. A different
> worker B reclaims (`lease_epoch` increments, `running → claimed`), calls `reconstructHistory`,
> reuses the committed decision (its **first** logged action is a `tool_call`, not a `plan`),
> runs the remaining iterations, and completes — over `https://4orge.duckdns.org`.
> `GET /jobs/{id}/trace` shows every `plan`/`tool_call` step executed **exactly once**
> (distinct `step_number` == count, no gaps), and the per-step `worker_id` shows **two**
> different workers — the cross-worker agent attribution the Week-3 demo narrated but couldn't
> surface per step. A poison-message agent that never `finish`es lands in `dead_letter` after
> `AGENT_MAX_STEPS × max_attempts`, having burned no more than a bounded number of LLM calls.
>
> **✅ Demonstrated live over HTTPS (2026-07-31, `DEMO_EXIT=0`):** job
> `78e30238-1846-454c-8f89-935127e2fb4b` (Groq backend). The killed worker (`worker-1`) was
> SIGKILL'd after 2 committed steps — `kill mode: post_iteration` (a full plan+tool_call iteration
> had landed, the next plan had not) → a different worker (`worker-3`) reclaimed (`lease_epoch`
> bumped, `running → claimed`), called `reconstructHistory`, resumed the loop, and completed at
> 9 steps. Because the kill landed *between* iterations, worker-3's first committed step was a
> fresh `plan` — a valid cross-worker recovery, though not the zero-re-spend money shot (that
> fires when the kill lands in the `[plan-committed, tool-committed)` window → `mid_iteration`).
> `trace` = 9 rows (`plan`+`tool_call`), contiguous `step_number` 1..9, each committed exactly
> once, `worker_id` spanning `worker-1` and `worker-3` (`worker_boundaries=[2]`); the final `plan`
> is a durable `finish` decision. Script printed `PASS: kill -9 -> different worker resumed agent
> from checkpoint, exactly-once, two-worker trace`.

---

## Verification (run end-to-end, locally + on the Oracle VM)

### Local (Windows dev box + docker-compose Postgres)
```bash
go vet ./... && go build ./...                              # Phase 0+ compiles
go test -race ./...                                         # everything, incl. new llm/tools/agent
go test -race -count=5 ./internal/worker/...                # chaos fuzz UNCHANGED (must stay green)
# Windows: tools tests skip run_tests when RUN_TESTS_PYTHON (python3) is unset — that is expected.
# Set RUN_TESTS_PYTHON=python (not python3) on Windows if you want run_tests to actually execute.

# Spin up the local stack and smoke the dispatcher + agent end-to-end:
docker compose up -d                                         # orchestrator + worker-1..4 + postgres + caddy
docker compose exec worker-1 sh -c 'python3 --version'      # python present for run_tests
# A segments job still works (backwards-compat): should complete, trace rows carry worker_id
curl -s localhost:8080/jobs -H 'content-type: application/json' \
  -d '{"task_type":"segments","payload":{"segments":5},"priority":0}'

# A cp_solve job with a local Ollama (OLLAMA_HOST default localhost:11434, an installed model):
curl -s localhost:8080/jobs -H 'content-type: application/json' \
  -d '{"task_type":"cp_solve","payload":{"problem":"two-sum","language":"python"},"priority":0}'
# poll GET /jobs/{id}/trace → expect plan / tool_call rows; GET /jobs/{id} → completed
```

### The thesis test (no live LLM needed)
```bash
go test -race -run TestAgentLoop_ResumeMidIterationAfterDepose -v ./internal/agent
# Asserts: no LLM re-call on resume (FakeBackend.CallCount()==2), countingTool ==1, trace exactly-once, completed.
```

### On the Oracle VM (live HTTPS demo — Task 7.3)
```bash
# SSH ubuntu@4orge.duckdns.org -i ~/.ssh/forge_vm ; cd ~/forge (on main)
git pull origin main
for f in migrations/*.up.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"; done   # applies 000004
docker compose build worker orchestrator && docker compose up -d
bash scripts/cp_solve_agent_demo.sh        # hits https://4orge.duckdns.org → prints PASS (exit 0)
```
CI (`go test -race ./...` + the `-count=5` chaos fuzz) keeps passing — `chaos_test.go` is byte-identical,
CI's `migrations/*.up.sql` glob auto-applies `000004`, and the new packages are covered by `go test -race ./...`.

---

## What comes next (Week 5 preview)

- **U9 — cost-aware rate limiting at the LLM boundary.** The `Usage{PromptTokens, CompletionTokens}` recorded
  in `CompleteResponse` this week becomes the input to a token-bucket limiter whose tokens are the *estimated
  token cost per LLM call* (debited against the provider's real free-tier budget — e.g. Groq tokens/min).
  In-memory first, then Upstash Redis-distributed so multiple worker processes share one budget. The retry client
  stays (transient-error resilience), the limiter wraps it (cost/throughput control) — two separable concerns that
  this week kept cleanly apart so Week-5 can bolt the limiter on without restructuring `internal/llm`.
- **U8 (Week 6)** turns the pristine ctx propagation into real OpenTelemetry: a trace per agent job, a span per
  `plan`/`tool_call`/`CompleteJob`, with a *cross-worker* trace of `claim@A → steps → kill → claim@B → resume →
  finish` — the trace that the `worker_id` attribution this week makes visible in the API.
- **U10 (Week 7)** injects the `Clock` interface + grows `FakeBackend` into the deterministic-time simulation that
  makes the Phase-6 chaos test run in milliseconds with no sleeps — turning the agent-loop invariants from
  "pass this run" into "provably always pass."

