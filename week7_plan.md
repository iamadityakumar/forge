# Week 7 Plan — Deterministic-time simulation: proof over flake

> Thesis: Weeks 1–6 made Forge **correct** (atomic claiming, fencing), **crash-
> recoverable** (lease + reclaim + resume), **intelligent** (LLM agent),
> **safe** (cost-aware rate limiting), and **observable** (metrics, logs,
> dashboard). Week 7 makes it **provable** — every distributed-systems claim
> the project rests on (the lease-expiry-while-alive race, the fencing-token
> depose, backoff/retry timing, exactly-once-under-crash) becomes a
> **deterministic simulation test** that fast-forwards an injected clock and
> reproduces the race in milliseconds — no `time.Sleep`, no 2-minute lease, no
> flake. The U7 chaos test runs in **virtual time under `-race` in CI on every
> commit**, so the Week 3 crash-recovery thesis is no longer a demo you record
> once — it is a property test that passes forever.

The textbook move is "write more tests and hope CI is fast." The senior move is
*make time a dependency*: Temporal and Redis don't test time-dependent
correctness with sleeps — they inject a `Clock` and *replay* scenarios against
virtual time. Week 7 does exactly that: it promotes Week 5's
`Clock`/`ManualClock` seed (`internal/ratelimit/clock.go`) into a first-class
seam threaded through the store (whose SQL `now()` becomes an injected `$now`),
the worker's lease-extender and claim loop (real timers become virtual timers),
the agent's step loop, and the LLM fake (which becomes a clock-aware simulator).
Then the three races the entire queue exists to survive — the paused worker
whose lease expires *while it is alive* (U2), the deposed zombie whose writes
are silently fenced (U1), the failed job whose backoff gates reclaim (U5) —
are each reproduced as a **named, green assertion in under 100 ms**.

The money shot: `go test -race -count=5 ./internal/worker/... ./internal/sim/...`
passes in seconds on a laptop **and in CI**, printing one line per race —
`TestSim_LeaseExpiryWhileAlive: fenced in 12ms, 0 double-steps` — the entire
distributed-systems story as a flake-proof test suite you can afford to run on
every commit.

---

## Section 0 — Where we are now

### Completed so far

| Week | Thesis | Evidence |
|---|---|---|
| 1 | Job API + durable Postgres queue | `POST /jobs`, `GET /jobs`, chi router, `000001_initial` migration |
| 2 | Atomic claiming + fencing | `SELECT … FOR UPDATE SKIP LOCKED`, `lease_epoch` fence token, `000003_fencing_checkpoints` |
| 3 | Crash recovery | Self-renewing lease (`executeWithLease`/`extenderLoop`), reclaim of expired `running` jobs, `000004_step_worker_attribution`, invariant chaos test (`chaos_test.go`) |
| 4 | LLM agent + durable checkpoint loop | `LLMBackend` abstraction, `cp_solve` agent with `search_kb`/`run_tests`, `plan`/`tool_call` step protocol, **live HTTPS crash-recovery demo on 2026-07-31** (`scripts/cp_solve_agent_demo.sh`, two-worker trace, exactly-once steps) |
| 5 | Cost-aware rate limiting at the LLM boundary | `internal/ratelimit/` token buckets, `RateLimitedBackend` estimate-then-reconcile decorator, `MAX_PENDING_JOBS` admission control, **`Clock`/`ManualClock` seed** (`internal/ratelimit/clock.go`), `scripts/burst_load_test.sh` |
| 6 | Observability: structured logging, metrics, dashboard | `internal/log` (`LOG_FORMAT`/`LOG_LEVEL`), `internal/metrics` on `prometheus/client_golang` with bounded cardinality, per-process `/metrics` (9091) + enriched `/health`, `web/` dashboard with live job timeline, **deployed live over HTTPS** + `docs/week6_demo.md` (U8 OTel tracing stretch **not** landed — no `opentelemetry` dep in `go.mod`) |

Week 7 is the **capstone verification week**. It adds no runtime feature — it
replaces the last unprovable part of the stack: the tests that depend on real
time. Weeks 1–6 built the machinery; the chaos test that proves the machinery
flaked when CI was loaded, and the three headline races (U1 fencing, U2
lease-as-heartbeat, U5 backoff) had **no focused test at all** — they were only
exercised by the slow, seeded, probabilistic chaos run. Week 7 threads one
`Clock` through the exact seams those weeks built and converts every
timing-dependent test into a deterministic simulation. **Zero new runtime
infrastructure, zero new dependencies** — the system you deploy in Week 7 is
byte-for-byte the Week 6 system, plus an injected clock parameter.

### Inherited scaffold (usable as-is)

- **`Clock`/`ManualClock`** (`internal/ratelimit/clock.go`) — the U10 seed
  planted in Week 5. `Clock{ Now() time.Time }`, `SystemClock`, thread-safe
  `ManualClock` with `Advance(d)`. Today it is used only inside `internal/ratelimit`
  tests; Week 7 promotes it and extends the interface with **virtual timers**
  (`After`, `NewTicker`, `Sleep`) so the *processes* — not just the data — become
  replayable.
- **`FakeBackend` with `CallCount`** (`internal/llm/fake.go`, Week 4) — the U10
  `FakeLlm` seed: deterministic, network-free LLM with scripted responses. Week 7
  adds clock-driven per-call delay, scripted failure sequences (to drive retry
  paths), and a per-(job,step) execution-count oracle.
- **The in-memory `chaosStore`** (`internal/worker/chaos_test.go`) — a faithful
  protocol emulation of Postgres fencing + `SKIP LOCKED` claim + reclaim-running +
  lease expiry, with an `execCount` exactly-once oracle and a seeded killer. It
  already satisfies `store.JobStore` (compile-time asserted). Its `now()` is the
  **one line** that makes the chaos test real-time; swapping it for the injected
  clock turns the whole test deterministic.
- **SQL is already parameterized** — every fenced write (U1/U4) uses `$1..$N`
  bind params. The only wall-clock call in the critical queries is SQL `now()`.
  Replacing `now()` with a `$now` bind sourced from the injected clock is
  mechanical and preserves the exact fencing semantics.
- **`run_at` already exists** (U5) — the backoff/scheduled-delay gate; the claim
  subselect already filters `run_at <= now()`. Deterministic backoff tests just
  need `run_at` computed from the injected clock — no schema change.
- **`StepsResumed` + per-step `worker_id` attribution** (Week 6, migration
  `000004`) — the trace-level evidence that a reclaimed job resumed from a
  checkpoint; the sim's assertions lean on it.

### Absent (greenfield for Week 7)

- **No clock outside `internal/ratelimit`** — the store, worker, and agent read
  wall-clock (`time.Now()`, SQL `now()`) directly. Threading the seam is the
  week's core work.
- **No virtual timer** — `ManualClock` cannot fire `After`/`Ticker`/`Sleep`, so a
  worker's lease-extender and claim loop cannot yet be driven deterministically.
- **No simulation harness** — the three canonical races are not reproduced as
  focused deterministic assertions anywhere.
- **CI runs the chaos test in real time** — `ci.yml` already runs
  `go test -race -count=5 ./internal/worker/...`, but the test paces itself on
  real `time.Sleep`, so under a loaded runner it is slow and occasionally flakes;
  there is no `internal/sim`, no deterministic race tests, and no virtual time.
- **`FakeBackend` has no failure/delay scripting** — retry and re-reclaim paths
  can't be driven end-to-end deterministically.
- **No `internal/clock/` package, no `internal/sim/` package** — both are new.

---

## Time & scope

- **Budget:** ~10–12 focused hours.
- **Core (must):** Phase 0 (Clock seam), Phase 1 (store `$now` injection),
  Phase 2 (worker/agent virtual timers), Phase 3 (deterministic FakeLlm),
  Phase 4 (simulation harness + three race tests), Phase 5 (chaos under `-race`
  in CI + test hygiene), Phase 7 (verification + docs).
- **Stretch (if time):** Phase 6 (per-attempt retry/backoff observability — the
  hardening Week 6 explicitly seeded for Week 7).
- **Order dependency:** Phase 0 → 1 → 2 → 3 → 4 is a hard chain — the sim needs
  the clock threaded through store, worker, agent, and LLM first. Phase 5 depends
  on Phases 0/2 (virtual-time chaos). Phase 6 is designed to drop without
  touching any core phase. No phase touches the deployed system's behavior.

---

## Phase 0 — The Clock seam (`internal/clock/`)

### Goal

One production-grade time abstraction with two implementations: `SystemClock`
(real time) and `ManualClock` (a virtual scheduler that fires timers when
advanced). Promote Week 5's `Clock`/`ManualClock` out of `internal/ratelimit`
into a shared package, **extend the interface with virtual timers** — a two-
method `Now()` clock can read time deterministically, but it cannot *replay the
process*: the worker's lease-extender ticks on a real `time.Ticker`, and a
ticker that isn't driven by the clock is a goroutine the simulation can't
control. `After`, `NewTicker`, and `Sleep` are the difference between stubbing
time and *replaying the system* (the Temporal Go SDK's `VirtualTime` test
framework and Redis's injected `struct time` both model this).

Keep `internal/ratelimit` compiling unchanged by re-exporting aliases — the
Week 5 limiter and its tests must not care that the clock moved house.

### Files

| File | Change |
|---|---|
| `internal/clock/clock.go` (new) | `Clock` interface (`Now`, `After`, `NewTicker`, `Sleep`), `SystemClock`, `Ticker` interface |
| `internal/clock/manual.go` (new) | `ManualClock` with a min-heap event queue, `NewManualClock(t)`, `Advance(d)` that fires due timers, `Pump()`/`WaitSettled` helpers for test sync |
| `internal/clock/clock_test.go` (new) | virtual-timer semantics: `After` fires on `Advance`, `Ticker` fires N times then stops, `Sleep` unblocks exactly at the deadline |
| `internal/ratelimit/clock.go` (rework) | delete the structs; keep `type Clock = clock.Clock`, `type SystemClock = clock.SystemClock`, `type ManualClock = clock.ManualClock`, `NewManualClock = clock.NewManualClock` so every existing caller compiles untouched |
| `go.mod` | unchanged (stdlib only — deliberately no `benbjohnson/clock`, no `testingclock`) |

### Behavior

```go
package clock

// Ticker abstracts time.Ticker so ManualClock can own the channel.
type Ticker interface {
    C() <-chan time.Time
    Stop()
}

type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
    NewTicker(d time.Duration) Ticker
    Sleep(d time.Duration)
}

// SystemClock is real time.
type SystemClock struct{}
func (SystemClock) Now() time.Time                 { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (SystemClock) NewTicker(d time.Duration) Ticker        { return realTicker{t: time.NewTicker(d)} }
func (SystemClock) Sleep(d time.Duration)                   { time.Sleep(d) }
```

`ManualClock` keeps a min-heap of `(fireAt, ch chan time.Time)` events plus a
current `now`. `Advance(d)` moves `now` forward and fires **every** event with
`fireAt <= now` in timestamp order, sending once on each channel (buffered
size 1). `After` schedules a one-shot; `NewTicker` schedules a repeating event
until `Stop()`; `Sleep` schedules an event and blocks the calling goroutine
until `Advance` fires it. Because `Advance` returns synchronously, a test
driver controls exactly when each blocked goroutine wakes, then yields
(`runtime.Gosched`) to let it observe the fire — the standard virtual-time
pump.

- `ManualClock` is goroutine-safe (one mutex guards `now` + the heap), so the
  `-race` detector still sees any unsynchronized access in the *real* code under
  test.
- `Pump()` (a bounded `runtime.Gosched` loop) and `WaitSettled(pred)` are test
  conveniences in the same package; production code never needs them.

### Interview angle

"A system that depends on time should declare time as a dependency. The seam is
an interface, not a flag: production passes `SystemClock`, tests pass a
`ManualClock` and *replay* time — the same move Temporal and Redis make to test
their own schedulers. And it has to be a *full* clock: a `Now()`-only fake
freezes the data but leaves the worker's lease-extender ticker on real time, so
the two-worker race you're trying to reproduce still races the Go scheduler.
`After`/`NewTicker`/`Sleep` in the seam are what make the *process* — not just
the timestamp — deterministic."

### Acceptance

- `internal/ratelimit` and its tests compile and pass **unchanged** after the
  move (aliases).
- `NewManualClock(t0); clk.Advance(5s)` fires a pending `After(5s)` exactly once,
  and a `NewTicker(2s)` fires 2× with `Now()` reading `t0+2s`, `t0+4s`.
- A goroutine blocked in `clk.Sleep(3s)` unblocks only when `Advance(3s)` is
  called (assert it is *still* blocked at `Advance(2999ms)`).
- Zero `time.Sleep` in `internal/clock` tests.

---

## Phase 1 — Thread the clock through the store (`internal/store/`)

### Goal

The store's view of time comes from the injected clock, not from Postgres'
`now()`. Every time-sensitive query binds a `$now` parameter sourced **once per
method** from `s.clk.Now()`; the fencing, `SKIP LOCKED`, and checkpoint
semantics are untouched. This is what makes lease expiry, backoff gates, and
`run_at` scheduling *replayable* against the real Postgres store.

### Files

| File | Change |
|---|---|
| `internal/store/store.go` | `New(...)` gains a `clock.Clock` param (or a `WithClock(clk)` option with `SystemClock` default); `JobStore` interface unchanged |
| `internal/store/postgres.go` | replace SQL `now()` with `$now` in **claim subselect**, **claim update**, **StartJob**, **RenewLease**, **CompleteJob**, **MarkFailed** (requeue `run_at` + dead-letter), **Release**; `now := s.clk.Now()` at the top of each method |
| `internal/store/postgres_test.go` | deterministic tests: inject `ManualClock`, advance, assert lease/backoff/claim timing against real Postgres |
| `cmd/orchestrator/main.go` | pass `clock.SystemClock{}` to `store.New` |
| `cmd/worker/main.go` | pass `clock.SystemClock{}` to `store.New` |

### Behavior

The audit list — every query whose semantics depend on wall-clock, all of which
already exist as parameterized SQL (U1–U5):

- **claim subselect:** `WHERE status='pending' OR (status IN ('claimed','running')
  AND lease_expires_at < $now)` `AND (run_at IS NULL OR run_at <= $now)`.
- **claim update:** `SET status='claimed', claimed_by=$worker,
  lease_expires_at=$now+$lease, lease_epoch=lease_epoch+1 WHERE … RETURNING lease_epoch`.
- **StartJob:** `SET status='running', started_at=$now WHERE … AND lease_epoch=$e`.
- **RenewLease:** `SET lease_expires_at=$now+$interval WHERE id=$1 AND lease_epoch=$e
  AND status IN ('claimed','running')` (the U2 heartbeat).
- **CompleteJob / MarkFailed / Release:** `completed_at=$now`, `run_at=$now+backoff`
  or `dead_letter`, `lease_expires_at=$now`.

One clock value per method — a claim and the subsequent `StartJob` must agree on
`now` so time never moves backward within a transition. The backoff formula stays
in Go (`base * 2^(attempt-1)`, capped, **plus jitter**) computed from `s.clk.Now()`
so the test can bound the jitter window exactly.

The deterministic backoff test that Phase 1 unlocks (this would previously have
required a real wait for the backoff duration):

```go
clk := clock.NewManualClock(t0)
s := newTestStoreWithClock(clk) // docker-compose Postgres, same harness as postgres_test.go
id, _ := s.CreateJob(ctx, "summarize", payload, 0, "")

job, epoch, err := s.ClaimJob(ctx, "w1", lease)   // epoch=1, lease_expires_at=t0+lease
s.StartJob(ctx, id, epoch)
s.MarkFailed(ctx, id, epoch, "boom")              // attempt 0→1, run_at = t0 + base + jitter

clk.Advance(backoff1 - time.Nanosecond)           // just before run_at
if _, _, err := s.ClaimJob(ctx, "w2", lease); !errors.Is(err, store.ErrNoJobs) {
    t.Fatalf("claimed before run_at: %v", err)    // backoff gate enforced
}
clk.Advance(time.Nanosecond)                      // exactly at run_at
job2, _, err := s.ClaimJob(ctx, "w2", lease)
if err != nil || job2.AttemptCount != 1 { t.Fatalf(...) }
```

A second Phase-1 test proves the U2 renew path deterministically: claim, advance
to `lease/2`, renew — `lease_expires_at` moves to `now+lease`; then *skip* a renew,
advance past expiry, and assert the job is reclaimable. The existing
`postgres_test.go` lease-expiry test (which today backdates with a raw
`UPDATE … now() - interval '1 second'`) is rewritten to drive the ManualClock
instead of hand-bending the database.

### Interview angle

"Postgres `now()` is the last hidden global. Binding `$now` from an injected
clock makes the store's entire time-dependent behavior — lease expiry, backoff,
scheduled `run_at` — a pure function of one value I can fast-forward. The hard
part was *not* swapping `now()` for `$now`; it was keeping every fence intact:
the `SKIP LOCKED` claim, the epoch check, the `FOR UPDATE` checkpoint CTE all
keep their exact SQL. A store that is *replayable* is a store whose correctness
you can prove."

### Acceptance

- `grep now()` against `internal/store/postgres.go` returns only server-side
  timestamps explicitly out of scope (none expected in claim/renew/start/
  complete/fail/release).
- `TestBackoffGatesReclaim` (above) passes in < 50 ms with a `ManualClock` against
  real Postgres.
- The claim subselect still returns `pending` ∪ expired `claimed`/`running`,
  `run_at`-gated — identical semantics, verified by the pre-existing tests.
- `cmd/orchestrator` and `cmd/worker` build with `clock.SystemClock{}` injected.

---

## Phase 2 — Thread the clock through worker + agent (virtual timers)

### Goal

The worker's *process* becomes replayable: the claim loop's poll delay and the
lease-extender's ticker fire on the injected clock instead of real timers, so a
whole multi-worker fleet can be driven forward by `Advance` calls. The agent's
step timing reads the same clock, so a "long LLM call" is a clock delta, not a
real wait. Production behavior is byte-identical — `SystemClock` wraps the same
`time` primitives — but the tests can now *control* when a lease renews, when a
claim is retried, and when a worker is frozen.

### Files

| File | Change |
|---|---|
| `internal/worker/loop.go` | the `Worker` type + `New(...)` (verify which file defines it — `loop.go`/`execute.go` on this branch) gains `clk clock.Clock`; plumb into the loop and executor |
| `internal/worker/loop.go` (or the file hosting `extenderLoop`) | claim poll delay → `clk.Sleep(poll)`; `extenderLoop` → `clk.NewTicker(lease/3)`; zero direct `time.NewTicker`/`time.Sleep` |
| `internal/worker/execute.go` | duration + renewal timestamps via `clk.Now()`; fenced `RenewLease` still passes `now` from the clock |
| `internal/agent/agent.go` | `New(...)` gains `clk clock.Clock`; step timestamps + `RunAt` via `clk.Now()` |
| `internal/worker/execute_test.go`, `internal/worker/loop_test.go` | convert the `time.Sleep(3ms/120ms)` synchronization to clock-advance + `Pump()` (see Phase 5) |
| `cmd/worker/main.go` | pass `clock.SystemClock{}` |

### Behavior

`extenderLoop` becomes:

```go
func (w *Worker) extenderLoop(ctx context.Context, jobID uuid.UUID, epoch int, lease time.Duration) {
    t := w.clk.NewTicker(lease / 3)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C():
            err := w.store.RenewLease(ctx, jobID, epoch, w.clk.Now(), lease)
            if errors.Is(err, store.ErrFenced) { // deposed: stop renewing AND stop pinning
                return
            }
        }
    }
}
```

- `RenewLease` now takes `now time.Time` as a parameter (from Phase 1) instead
  of deriving it; the worker passes `w.clk.Now()`.
- The claim loop's empty-poll `clk.Sleep(pollInterval)` is what the sim advances
  through to make a newly-eligible job get claimed "in the next poll".
- The agent's step loop records `started_at`/`completed_at` on each checkpointed
  step via `clk.Now()`; the real `RUN_TESTS_TIMEOUT_MS` (a tool-boundary timeout)
  stays on real time — it is not a queue-semantics seam.

The key testability property: with a `ManualClock`, a worker can claim, extend
its lease, run steps, and complete a job **purely by advancing the clock** — no
real waiting anywhere. Phase 2's acceptance test drives that end-to-end.

### Interview angle

"A `Now()`-injected store makes the *data* deterministic; it doesn't make the
*process* deterministic. The lease-extender is a goroutine on a real `time.Ticker`
— until that ticker ticks on the injected clock, you cannot replay a
two-worker reclaim. Moving the worker's timers into the seam is the step that
turns a mock clock into a *simulation*: the fleet, the pauses, and the failures
all become events on one timeline you control."

### Acceptance

- `grep -E "time\.(Now|After|NewTicker|Sleep)" internal/worker/` returns nothing
  (the segment-work simulation in `execute.go` becomes clock-sleep too).
- A `ManualClock`-driven worker claims, extends (renew fires at `lease/3`),
  completes a job with **no real sleep**; assert `RenewLease` was called ≥ 1 with
  `now` values strictly increasing by the lease/3 cadence.
- `internal/worker` tests pass; `internal/agent` tests pass.

---

## Phase 3 — Deterministic FakeLlm (`internal/llm/fake.go`)

### Goal

The LLM dependency becomes a **scripted, clock-aware simulator**: per-call
latency expressed in virtual time, scripted success/failure sequences that drive
the retry and re-reclaim paths deterministically, and a per-(job,step)
execution-count oracle — the safety counter that makes exactly-once an
assertion instead of a hope. `FakeBackend`'s Week-4 `CallCount` API is kept so
existing tests compile.

### Files

| File | Change |
|---|---|
| `internal/llm/fake.go` (rework) | add `Delay(clk clock.Clock, d)` (implemented as `clk.Sleep(d)`), scripted per-index responses/failures (`Script{…}`), `StepCalls(jobID uuid.UUID, step int) int` oracle, retain `CallCount` |
| `internal/llm/llm.go` | `LLMBackend` interface unchanged |
| `internal/llm/ratelimited_test.go`, `internal/agent/agent_test.go` | drive the new scripted fake through backpressure + retry deterministically |

### Behavior

```go
fb := llm.NewFakeBackend().
    Script(0, okResp("step-1 plan")).      // call index → response
    Script(1, llm.ErrRateLimited).         // force a retry/backpressure path
    Script(2, okResp("step-2 plan")).
    Delay(clk, 250*time.Millisecond)       // virtual latency, NOT time.Sleep
```

- `Complete` consumes the script in order; on a scripted error it returns the
  error and records it; the delay is `clk.Sleep(d)`, so the sim fast-forwards
  through "the LLM is slow" instead of waiting.
- `StepCalls(jobID, step)` returns how many times the fake was asked to produce
  that step — the exactly-once oracle used by Phase 4's sim assertions. Under
  correct fencing + resume, a reclaimed job's step is requested at most once; a
  double-execute (fencing removed) drives the counter to 2.
- Backpressure path: when the `RateLimitedBackend` wait is in effect, the
  combined decorator + fake delay both express themselves in clock time, so the
  backoff/rate-limit interplay is testable in virtual time.
- The real `retryTransient` backoff in `internal/llm/llm.go` (a real
  `time.After`) is **short-circuited by the fake**: scripted errors return
  directly, so retry timing is whatever the sim's script says — the real timer
  never enters the picture. (Production retry stays untouched; this is purely
  how the fake bypasses it.)

### Interview angle

"The queue's last uncontrolled input is the LLM — nondeterministic latency, and
failures that only sometimes happen. A fake that sleeps in real time still lets
races through; a fake that sleeps on the injected clock makes the agent's
`plan → tool_call → observe` loop a script you can replay, failure by failure.
The execution-count oracle is the same trick as the chaos test's `execCount`:
make the 'never twice' guarantee *countable*, and a fencing regression shows up
as a number, not a flake."

### Acceptance

- A scripted `ErrRateLimited` at call index 1 drives the agent's retry/backpressure
  path deterministically (assert attempt/call count increments, then succeeds).
- `Delay(clk, 250ms)` adds exactly 250 ms of virtual time (assert via `clk.Now()`
  deltas) and **zero** real wall-clock.
- `StepCalls(jobID, step) ≤ 1` across a simulated reclaim in Phase 4.

---

## Phase 4 — Simulation harness + the three canonical races (`internal/sim/`)

### Goal

A tiny harness (`internal/sim/`) that runs **N worker goroutines + M jobs**
against a clock-driven store, and three named tests that deterministically
reproduce the exact races the project exists to survive — each a green assertion
in under 100 ms with no sleeps. This is the week's thesis made test.

The harness is deliberately small and dependency-free: workers are the real
`internal/worker.Worker` wired to a `ManualClock`; the store is either the
real-Postgres store (Phase 1) or — for CI without Postgres — the in-memory
protocol store that `chaos_test.go` already maintains (moved into the package so
both the chaos test and the sim share it). The sim driver advances the clock,
pumps goroutines, and scripts the pauses/kills/failures.

### Files

| File | Change |
|---|---|
| `internal/sim/sim.go` (new) | `Run(t, store, clk, numWorkers, script)` — spawn workers on the ManualClock, advance + `Pump()` in lockstep with a scenario script, wait for quiescence |
| `internal/sim/sim_test.go` (new) | `TestSim_LeaseExpiryWhileAlive`, `TestSim_FencingTokenRace`, `TestSim_BackoffTiming` |
| `internal/worker/chaos_test.go` (rework) | move the in-memory `chaosStore` (now clock-injected, Phase 5) into a shared spot the sim can use — or keep it in `worker` and let `sim` import a small `simstore` package |
| `internal/worker/chaosStore` (move) | `store.JobStore` emulation with `execCount` oracle + a `now() → clk.Now()` seam |

### Behavior — the three deterministic race tests

**1. `TestSim_LeaseExpiryWhileAlive`** (U2/U3 — the paused worker whose lease
expires *while it is alive*):

```text
A claims J (epoch=1), completes step 1, starts step 2.
Driver SUSPENDS A's extenderLoop  (A alive, but not renewing — the GC-pause/VM-suspend)
clk.Advance(lease)                → lease_expires_at passes
B claims J (epoch=2)              → resumes from last completed step (=1)
A's next RecordStep/RenewLease    → 0 rows → ErrFenced → A abandons
```

Assert: exactly-once steps (`StepCalls`/`execCount` ≤ 1 per (job,step)), J
completes, and A observed `ErrFenced` (the sim logs it and the test asserts it).
This is the failure mode **no** textbook queue handles — the textbook claim
query only reclaims `pending`/`claimed` and would leave the job stuck in
`running`; Forge's U3 reclaim condition is exactly what the test proves.

**2. `TestSim_FencingTokenRace`** (U1 — the deposed zombie's writes are fenced):

```text
A claims J (epoch=1).
clk.Advance(lease); B claims J (epoch=2); B runs step 1..K, completes J.
A (zombie, holding epoch=1) attempts CompleteJob and RecordStep afterwards.
```

Assert: A's `CompleteJob` and `RecordStep` return `ErrFenced` (0 rows) and **J's
`completed` transition happened exactly once** — the epoch fence, not luck,
prevented double-execution. Remove the `AND lease_epoch=$e` and this test goes
red — that is the point.

**3. `TestSim_BackoffTiming`** (U5 — a failed job's retry is gated by `run_at`,
and the jitter is bounded):

```text
A fails J at attempt 1 → MarkFailed requeues with run_at = now + backoff(1) + jitter
clk.Advance(backoff1 - 1ns)      → no worker can claim J (gate enforced)
clk.Advance(1ns)                 → exactly at run_at → B claims; attempt_count == 1
```

Assert: claimability flips exactly at `run_at`; `run_at - now ∈ [backoff1,
backoff1 + jitterMax]` (jitter bounds verified); attempt_count increments; a
second failure follows the `base * 2^1` schedule.

The pump pattern shared by all three:

```go
clk.Advance(step); sim.Pump()    // runtime.Gosched + WaitSettled — let fired timers wake their goroutines
// then assert invariants against the store + oracle
```

### Interview angle

"A seeded chaos test is probabilistic — it proves the invariant *for that seed*.
A simulation is the same test where the pauses, kills, and failures are **events
on a timeline**, so a failing seed is a failing *scenario you can name and rerun
deterministically*. These three tests are the three interview answers — U1
fencing, U2 lease-as-heartbeat, U3 reclaim-running, U5 backoff — expressed as
assertions, not anecdotes. That is how Temporal and Redis test their own
correctness, and it is the difference between 'the demo worked when I recorded
it' and 'the invariant holds, always.'"

### Acceptance

- All three tests pass under `go test -race -count=10 ./internal/sim/...` in
  **< 5 s total**.
- Each reproduces its race with a fixed seed; repeated runs produce identical
  assertion results (no timing flake) — verified in Phase 7's determinism check.
- A deliberate regression (remove the epoch fence / drop `running` from the
  reclaim condition) turns the corresponding test red — proven once in a review
  run, not shipped.

---

## Phase 5 — U7 chaos under `-race` in CI + test hygiene

### Goal

The existing U7 chaos test becomes **virtual-time** — finishing in milliseconds
instead of real seconds, immune to loaded-CI flake — and is wired into a GitHub
Actions workflow that runs it (and the sim) under `-race` on every push. As
hygiene, the remaining `time.Sleep`-based test synchronization across the repo
converts to clock-advance or channel sync, so the suite's wall-time is a
documented budget, not an accident.

### Files

| File | Change |
|---|---|
| `internal/worker/chaos_test.go` (rework) | `chaosStore.now()` → injected `clock.Clock`; inter-kill jitter delay → `clk.Sleep`; the `5ms` poll → clock-advance + `Pump()`; keep the three invariants verbatim |
| `internal/worker/ratelimit_chaos_test.go` (rework) | same treatment (`100ms` sleep → clock-advance) |
| `.github/workflows/ci.yml` (extend) | add `./internal/sim/...` to the race step; keep the existing chaos `-count=5` — which is now fast because it runs in virtual time |
| `internal/worker/loop_test.go`, `execute_test.go`, `internal/llm/*_test.go`, `internal/ratelimit/*_test.go` | convert `time.Sleep` sync to `clk`/`Pump` or channel barriers (Phase 2/3 already move the big ones) |

### Behavior

The chaos test keeps its three invariants — (1) liveness: every job reaches
`completed` or `dead_letter`; (2) safety: the `execCount` oracle proves no step
is committed more than once and steps are `{1..K}` with no gaps; (3) no panics,
no data races under `-race` — but every `time.Sleep` that previously paced the
killer and the poll is replaced by clock events. The seeded killer still uses
`rand.New(rand.NewSource(seed))`; the seed now selects a **deterministic event
script**, so a given seed replays identically.

The delta to the **existing** `ci.yml` (it already runs `go vet`, `go build`,
`go test -race ./...`, and chaos `-count=5`) is one line — add `./internal/sim/...`
to the race step and let the virtual-time chaos make the existing `-count=5`
cheap:

```yaml
- run: go test -race -count=3 ./...
- run: go test -race -count=5 ./internal/worker/... ./internal/sim/...
```

(Postgres-backed tests gate on `TEST_DATABASE_URL`; the sim + chaos tests run
with no DB, so the race step is green without the Postgres service.)

### Interview angle

"U7's contribution was 'a property test instead of a story.' Its weakness was
wall-clock dependence — under a loaded CI, `-count=5` took minutes and
occasionally flaked, which made it *affordable to skip*. Virtual time removes the
only remaining nondeterminism, so the thesis runs `-race -count=3` on every
commit in seconds. A property test you can afford to run continuously is a
property test that actually guards the system; one that takes five minutes under
load is a ticket to a red X you learn to ignore."

### Acceptance

- `go test -race -count=5 ./internal/worker/... ./internal/sim/...` passes in
  **< 30 s** locally (virtual time), down from real-time minutes.
- `.github/workflows/ci.yml` runs on every push and fails the build on any
  liveness/safety/race violation.
- `grep -r "time.Sleep" internal --include=*_test.go` returns only deliberately
  real-behavior tests (ideally none); the suite's wall-time budget (target:
  `go test ./...` < 60 s including Postgres-backed tests) is documented in the
  checkpoint.

---

## Phase 6 — Stretch: per-attempt retry/backoff observability

### Goal

Deepen Week 6's tracing seam — the hardening Week 6 explicitly seeded for Week 7:
a job that bounced through the retry/backoff path is explainable from its trace,
with the **attempt number, the computed backoff, and the `run_at` target**
recorded as span attributes. If Week 6's U8 OpenTelemetry tracing landed, this
extends those spans; if it did not, the attributes ride on the existing
`job_steps` record + structured log lines — the story stays the same.

### Files

| File | Change |
|---|---|
| `internal/worker/execute.go` (or `internal/store` caller of `MarkFailed`) | on each `MarkFailed` requeue, emit a span/log with `attempt`, `backoff_ms`, `run_at` |
| `internal/agent/agent.go` | on step failure that triggers retry, add `attempt` to the step span/log |
| (if U8 landed) `internal/trace/` | per-attempt span on the retry path |

### Behavior

A job failed at attempt 1 logs/spans:
`msg="requeued with backoff" job=… attempt=1 backoff_ms=4000 run_at=… attempt_count=1 max_attempts=3`
A job that exhausts attempts logs `attempt=3 … dead_letter=true`. `GET /jobs/{id}/trace`
already renders the ordered steps; the retry span makes the *gaps* — the backoff
waits — visible too.

### Interview angle

"Observability without retry context hides the second-most-common failure: 'why
did this job take 8 minutes when it has 3 steps?' Because it was requeued twice,
and the backoff sleeps aren't steps. Recording attempt/backoff/run_at on the
retry path closes that gap — the trace now explains *waiting*, not just *doing*."

### Acceptance (stretch)

- A scripted fail→requeue→succeed run produces a trace/log showing
  `attempt=1 backoff_ms=… run_at=…` then `attempt=2`.
- A poison job shows `attempt=3 … dead_letter=true`.
- No core phase depends on this phase; dropping it changes nothing.

---

## Phase 7 — Verification + docs

### Goal

Prove the whole thing: the deterministic suite green locally and in CI, the
determinism property demonstrated (same seed ⇒ same output), the flake budget
hit, and the story recorded. No new runtime behavior means **no forced redeploy**
— the deliverable is the test suite, not a feature.

### Behavior

```bash
go vet ./... && go build ./... && go test -race ./...
go test -race -count=10 ./internal/sim/...
go test -race -count=5 ./internal/worker/...
grep -r "time.Sleep" internal --include=*_test.go   # expect: no timing sleeps
```

Determinism proof: run the sim suite twice with the same seed and `diff` the
output — assertion lines are byte-identical (no `time.Now()` in test output).
Then run once with `-count=3` to show repeats are stable.

### Docs

- **`STANDOUT_UPGRADES.md`**: mark U10 delivered; the "How these ripple across
  the weeks" Week 7 row gets its status flip. No edit to the U7/U10 prose needed.
- **`docs/week7_demo.md`** (new): the money-shot transcript — the three race
  tests passing under `-race` with timings, the chaos `-count=5` pass, and the
  before/after flake story (real-time chaos minutes → virtual-time seconds).
- Optionally a `make test-fast` target (`-race -count=3` over sim/worker).

### Acceptance

- Full `go test -race ./...` green; sim + chaos suites under their time budgets.
- The three named race tests appear in CI output as distinct passing tests.
- Determinism diff is byte-identical across two runs.
- `docs/week7_demo.md` captures the transcript; `STANDOUT_UPGRADES.md` marks U10
  delivered.
- Live deploy untouched — rerun the Week 4/6 demos only to confirm no regression.

---

## Out of scope / seeds planted

- **No new runtime infrastructure, no new dependencies** — `internal/clock` is
  ~150 lines of stdlib; the sim and fake-Llm live in tests. No in-memory store
  replaces Postgres in production.
- **No behavioral change to the queue** — lease constants, backoff caps, fencing
  semantics, and the API are untouched; the deployed system is byte-identical.
- **Seed for a later week — scheduled/delayed jobs:** `run_at` is now fully
  clock-parameterized, so "submit a job runnable only after `T`" has a
  deterministic test waiting for it for free (STANDOUT_UPGRADES.md already calls
  this out).
- **Seed — `-race`-enabled CI for the whole suite** — the workflow in Phase 5 is
  the first one; extending it to Postgres-backed tests is a config change.
- **Deliberately not done:** a full Temporal-style deterministic *replay engine*
  (single-threaded, non-determinism detector). The ManualClock-driven sim covers
  the races this system has; a replay engine is the right tool for a much larger
  state machine and would out-size this week's scope.

## Progress tracking

| # | Task | Phase | Core/Stretch | Status |
|---|---|---|---|---|
| 7.0 | Promote `Clock` to `internal/clock` + aliases in `ratelimit` | 0 | Core | ☐ |
| 7.1 | Virtual timers: `After`/`NewTicker`/`Sleep` on `ManualClock` + event queue | 0 | Core | ☐ |
| 7.2 | `internal/clock` tests (fire-on-advance, ticker stop, sleep unblock) | 0 | Core | ☐ |
| 7.3 | Store takes `clock.Clock`; `$now` bound in claim/renew/start/complete/fail/release | 1 | Core | ☐ |
| 7.4 | `TestBackoffGatesReclaim` + rewrite the backdating lease test to ManualClock | 1 | Core | ☐ |
| 7.5 | Worker + agent take `clock.Clock`; extender ticker + poll sleep virtualized | 2 | Core | ☐ |
| 7.6 | End-to-end ManualClock worker claim→extend→complete with no sleep | 2 | Core | ☐ |
| 7.7 | FakeLlm: scripted responses/failures, `Delay(clk,d)`, `StepCalls` oracle | 3 | Core | ☐ |
| 7.8 | `internal/sim` harness (workers on ManualClock, pump, scenario driver) | 4 | Core | ☐ |
| 7.9 | `TestSim_LeaseExpiryWhileAlive` | 4 | Core | ☐ |
| 7.10 | `TestSim_FencingTokenRace` | 4 | Core | ☐ |
| 7.11 | `TestSim_BackoffTiming` | 4 | Core | ☐ |
| 7.12 | chaos test in virtual time (killer + poll on the clock) | 5 | Core | ☐ |
| 7.13 | `.github/workflows/ci.yml` — `go vet`/`build`/`test -race -count=3` | 5 | Core | ☐ |
| 7.14 | Convert remaining `time.Sleep` test sync to clock/pump | 5 | Core | ☐ |
| 7.15 | Determinism proof + `docs/week7_demo.md` + `STANDOUT_UPGRADES` U10 status | 7 | Core | ☐ |
| 7.16 | Per-attempt retry/backoff observability | 6 | Stretch | ☐ |

## Week 7 checkpoint (must be demo-able)

`go test -race -count=5 ./internal/worker/... ./internal/sim/...` passes in
seconds, in CI, on every commit. The three races — lease-expiry-while-alive,
fencing-token depose, backoff timing — are **named green tests**, each under
100 ms. The chaos test is deterministic in virtual time, still asserting exactly-
once under seeded kills. Zero timing `time.Sleep` remains in the unit suite.
The one-sentence story: **"I made time a dependency, and the distributed-systems
claims I've been making since Week 3 — fencing, lease-as-heartbeat, backoff,
exactly-once-under-crash — are now properties proved by tests that run in
milliseconds and never flake."**

## Verification

- **Local:** `go vet ./... && go build ./... && go test -race ./...` green.
- **Simulation:** `go test -race -count=10 ./internal/sim/...` — three named
  races pass in < 5 s total.
- **Chaos:** `go test -race -count=5 ./internal/worker/...` — three invariants
  hold in virtual time, < 30 s.
- **Determinism:** run the sim twice with the same seed; `diff` output is
  byte-identical.
- **Hygiene:** `grep -r "time.Sleep" internal --include=*_test.go` finds no
  timing sleeps; `go test ./...` wall-time under the documented budget.
- **CI:** `.github/workflows/ci.yml` green on the week-7 branch, including the
  sim + chaos packages under `-race`.
- **Live (regression only):** rerun the Week 4 crash-recovery demo and Week 5
  burst test against the unchanged deploy to confirm zero behavior drift.




