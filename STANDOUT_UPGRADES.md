# Forge — Standout Upgrades Over the Regular Crowd

> **Premise.** A "job queue in Go + Postgres with `SKIP LOCKED`" is a strong,
> well-trodden project — and that is exactly the problem. Hundreds of resumes
> build the textbook version: claim, execute, complete. The ceiling of that
> version is "I read a blog post." The floor of a senior version is "I found
> the failure modes the textbook skips, and I can defend every line against an
> interviewer who has run these systems in production."
>
> This document is the lens for the rest of the build. Each upgrade below
> replaces a textbook behavior with one that demonstrates real distributed-
> systems judgment, names the concept it embodies, and is provable by a test
> or a demo. Items marked **🟢 W3** are baked into `week3_plan.md` (the project's
> thesis week — "survive `kill -9`"). The rest land in later weeks.

---

## At a glance

| # | Upgrade | Replaces (the textbook version) | Interview concept | Lands in |
|---|---------|----------------------------------|-------------------|----------|
| U1 | **Fencing tokens (`lease_epoch`)** | SKIP LOCKED + lease expiry alone | Kleppmann fencing tokens; exactly-once-under-failure | 🟢 W3 |
| U2 | **Lease extension as the alive-signal** | Fixed lease; worker "death events" | Heartbeat/lease duality (Temporal activity heartbeats, Kafka `max.poll.interval.ms`) | 🟢 W3 |
| U3 | **Reclaim expired `running` jobs, not just `claimed`** | Claim only reclaims `pending`/`claimed` | Tracing the real failure path | 🟢 W3 (bug fix) |
| U4 | **Checkpointed, resumable, idempotent steps** | `job_steps` table exists but is unused | WAL/replay; zero-loss resumption | 🟢 W3 |
| U5 | **Retry + exponential backoff + jitter + dead-letter** | Failed jobs are dead-ended forever | SQS DLQ; Oban retry/discard; poison messages | 🟢 W3 |
| U6 | **Bounded per-worker concurrency (structured)** | One job at a time, serial loop | Go semaphores, errgroup, structured concurrency | 🟢 W3 (stretch) |
| U7 | **Invariant-based chaos test under `-race`** | A kill script that you eyeball | Property/invariant testing of probabilistic systems | 🟢 W3 (stretch) |
| U8 | **Distributed tracing (OpenTelemetry)** | Prometheus counters only | Cross-worker spans; real observability | W4–W6 |
| U9 | **Cost-aware rate limiting by estimated token spend** | Flat QPS limiting at the request boundary | Modeling real capacity (provider token budgets) | W5 |
| U10 | **Deterministic-time simulation (fake clock + fake LLM)** ✅ | `time.Sleep` in tests → flake | How Temporal/Redis test themselves | 🟢 **W7** |

**Also:** partial indexes on the claim query (smaller + faster, a real senior
touch), `run_at` enabling scheduled/delayed jobs for free, a
`GET /jobs/{id}/trace` + dashboard step-timeline (the demo's money shot),
graceful `Release` on `SIGTERM` for instant re-queue instead of waiting out the lease.

---

## U1 — Fencing tokens (`lease_epoch`)

**The problem the textbook skips.** `SKIP LOCKED` + a lease guarantees two workers
don't *claim* the same row. It does **not** guarantee two workers don't *execute* the
same job. Suppose worker A claims a job, then hits a long GC pause, gets
preempted, or its VM is suspended. A isn't dead — it's frozen. Its lease expires.
Worker B's claim subselect sees `lease_expires_at < now()`, reclaims the job, and
runs it. A thaws, still believing it owns the job, and:
- re-runs tool calls (wasted LLM tokens / double side effects),
- writes step checkpoints that collide with B's,
- calls `CompleteJob`, racing B's `CompleteJob`.

The textbook design has no defense: A has no way to learn it was deposed.

**The upgrade.** Add `lease_epoch INT NOT NULL DEFAULT 0` to `jobs`. **Every** claim
does `lease_epoch = lease_epoch + 1` and returns the new epoch to the claimer. The
claimer treats that epoch as a **fencing token**: every mutation it performs on the
job — `StartJob`, `RecordStep`, lease renewal, `CompleteJob`, `MarkFailed`,
`Release` — is executed with `AND lease_epoch = $my_epoch` in the `WHERE` clause.

If A was deposed, A's epoch ($n) no longer matches the row's epoch ($n+1). A's
writes affect **zero rows** — silently, atomically, no error storm — and A detects
"0 rows ⇒ I was fenced" and abandons the job. B, holding $n+1, proceeds
uncontested. Double execution is prevented *by construction*, not by luck.

```sql
-- fenced completion: a zombie A (old epoch) cannot complete a job B now owns
UPDATE jobs SET status = 'completed', completed_at = now()
WHERE id = $1 AND status = 'running' AND lease_epoch = $2;
-- A's call returns 0 rows affected → ErrFenced → A logs "fenced, abandoning".
```

**Interview concept:** Martin Kleppmann's *fencing tokens* (the canonical fix for
the leader-election zombie problem). This single feature is the difference between
"student queue" and "someone who reads the literature."

---

## U2 — Lease extension as the alive-signal

**The problem.** A fixed lease breaks on long jobs. The current code uses
`2 * time.Minute`. A multi-step LLM job in Week 4 can easily run 3–5 minutes. The
lease expires while the worker is *healthy*, B reclaims, and now A and B both run —
even with no crash at all. The queue actively creates the bug it's meant to prevent.

Conversely, making the lease "very long" trades correctness for slow recovery: a
truly dead worker holds its job hostage until the long lease elapses.

**The upgrade.** Treat *lease renewal* as the heartbeat. While a worker holds a job
(claimed → running), a **per-job goroutine** renews `lease_expires_at = now() + lease`
every `lease/3`, fenced by epoch:

```sql
UPDATE jobs SET lease_expires_at = now() + $2::interval
WHERE id = $1 AND lease_epoch = $3 AND status IN ('claimed','running');
```

- **Alive worker** ⇒ renews ⇒ lease never expires while healthy ⇒ no false reclaim.
- **Dead/paused worker** ⇒ stops renewing ⇒ lease expires ⇒ self-healing reclaim.
- **Zombie worker (deposed, fences itself)** ⇒ renewal returns 0 rows ⇒ the goroutine
  signals the executor to **abort immediately** — the zombie stops *and* stops
  pinning the job.

This collapses "worker presence" and "job ownership" into one mechanism. There is
no separate death-detection process, no stale-worker sweeper needed for progress
(the claim subselect is the sweeper). This is exactly the heartbeat/lease duality
that Temporal (activity heartbeats) and Kafka (`max.poll.interval.ms`) rely on.

**Interview concept:** "I extended leases, not heartbeats, because a lease renewal
*is* a heartbeat that also encodes ownership — and it's self-fencing, so a zombie
can't keep a job alive after it's been deposed."

---

## U3 — Reclaim expired `running` jobs (the bug hiding in the current query)

**The problem, found by reading the actual code.** The current claim subselect is:

```sql
WHERE status = 'pending'
   OR (status = 'claimed' AND lease_expires_at < now())
```

`StartJob` transitions `claimed → running`. So a worker that crashes **after**
`StartJob` leaves the job in `running`. **`running` is not in the reclaim
condition.** The job sits in `running` forever — no worker ever reclaims it, the
lease is irrelevant to the next claim. The crash-recovery demo *cannot work* against
the current query: kill a worker mid-job and the job is lost, not recovered.

**The upgrade.** Reclaim both post-`StartJob` states:

```sql
WHERE status = 'pending'
   OR (status IN ('claimed','running') AND lease_expires_at < now())
```

Reclaiming transitions `running → claimed` (the `UPDATE … SET status = 'claimed'`),
`lease_epoch` increments, and the new worker resumes from the last checkpoint
(see U4). This is the single change that makes Week 3's thesis actually true.

**Interview concept:** "The textbook claim query reclaims unclaimed jobs. I traced
where a crash actually leaves the row and found `running` was unrecoverable — so the
reclaim condition has to cover it."

---

## U4 — Checkpointed, resumable, idempotent steps

**The problem.** The `job_steps` table is in the schema (`migrations/000001`) but
**referenced by zero Go files** (verified). Today a job is an opaque unit — progress
is all-or-nothing. "Resume from the last step" is the project's *namesake guarantee*
and it currently doesn't exist.

**The upgrade.** Checkpoint after **every** step with a fenced write (a `CTE` that
only inserts the step row if the job's epoch still matches the caller's):

```sql
WITH owned AS (
    SELECT 1 FROM jobs WHERE id = $1 AND lease_epoch = $2 FOR UPDATE
)
INSERT INTO job_steps (job_id, step_number, step_type, input, output, status, duration_ms)
SELECT $1, $3, $4, $5, $6, 'completed', $7 FROM owned
ON CONFLICT (job_id, step_number) DO UPDATE
SET output = EXCLUDED.output, status = EXCLUDED.status, duration_ms = EXCLUDED.duration_ms
RETURNING id;
-- 0 rows returned ⇒ epoch mismatch ⇒ the caller was fenced ⇒ ErrFenced ⇒ abandon.
```

On reclaim, the new worker resumes from `MAX(step_number) WHERE status='completed'`
and starts at `+1` — not from the beginning. With U1's fencing, a zombie that
re-awakes cannot re-write or corrupt B's checkpoints. For Week 3 we use a **dummy
multi-step job** (K short segments, each checkpointed) so the crash-recovery demo is
real and testable *before* the LLM exists (Week 4 simply replaces the segment body
with a real plan→tool→observe step).

```
kill -9 worker mid-job at step 3/5
  → job left 'running', lease expires
  → worker B reclaims (running→claimed, epoch++)
  → B reads LastCompletedStep = 2
  → B executes steps 3,4,5 and completes
  → zero wasted work, no step executed twice
```

**Interview concept:** WAL/replay semantics — durable progress records that make
recovery *resumption*, not *restart*. Add `GET /jobs/{id}/trace` returning the
ordered steps; the dashboard renders the step timeline live as the job recovers.
**That replay is the demo's money shot.**

---

## U5 — Retry with backoff + jitter, and a dead-letter outcome

**The problem.** A failed job today goes to `status='failed'` and the claim
subselect never touches `failed` again — it is **permanently dead-ended**. There is
no retry, no backoff, no poison-message handling. `max_attempts` and
`attempt_count` exist in the schema but `attempt_count < max_attempts` is **never
enforced**.

**The upgrade.** A fenced `MarkFailed(ctx, jobID, epoch, reason)` that branches:

- `attempt_count < max_attempts` ⇒ requeue: `status='pending'`,
  `claimed_by=NULL`, `lease_expires_at=NULL`, `run_at = now() + backoff`,
  `error_message=reason`. Fast jobs retry quickly; the claim subselect's new
  `AND run_at <= now()` gate enforces the delay.
- `attempt_count >= max_attempts` ⇒ `status='failed'`, `dead_letter=true`,
  `error_message=reason`. Surfaced by `GET /jobs?status=dead_letter`.

Backoff = `base * 2^(attempt-1)` capped at e.g. 5 min, **plus jitter** to prevent
thundering-herd retry storms. `run_at` is reused for free as a *scheduled-jobs*
feature (submit a job runnable only after `T`).

**Interview concept:** SQS dead-letter queues, Oban's retry/discard, poison-message
handling — production queue semantics, not a toy.

---

## U6 — Bounded per-worker concurrency (structured)

**The problem.** `worker.Run` is a serial one-job-at-a-time loop. On a 4-OCPU ARM
node that leaves 3 cores idle while a single job waits on an LLM network call.

**The upgrade.** Each worker process runs up to `WORKER_CONCURRENCY` jobs
concurrently behind a `golang.org/x/sync/semaphore`, each job owning its own
lease-extension goroutine and fenced step loop, all rooted in one cancellable
context. Structured concurrency: when the worker's context cancels, every in-flight
job and its lease goroutine tear down cleanly.

**Interview concept:** fluent, idiomatic Go concurrency — the thing this language was
built for, used correctly (semaphores, child contexts, `errgroup`, bounded fan-out).

---

## U7 — An invariant-based chaos test under `-race`

**The problem.** A `kill_recovery_test.sh` you eyeball once proves it *worked that
time*, not that it *always works*. Flaky concurrency bugs hide behind "it passed when
I recorded the GIF."

**The upgrade.** A deterministic `go test -race` harness: M checkpointed jobs, N
worker goroutines, a **seeded** pseudo-random killer that cancels workers mid-step,
asserting three **invariants**:

1. Every job reaches `completed` or `dead_letter` (liveness — no job stuck forever).
2. **No step is ever executed more than once** (safety — exactly-once-under-crash),
   measured by an in-memory per-(job,step) execution counter in a fake tool.
3. No panics, no data races (the `-race` detector).

The headline invariant — **"exactly-once step execution under crash"** — is the
project's thesis, expressed as a passing test instead of a story. This is the
interview material you cannot fake.

**Interview concept:** property/invariant testing of a probabilistic system — how you
test a database, a scheduler, or anything with nondeterminism, instead of sampling it.

---

## U8 — Distributed tracing, not just metrics

Metrics tell you *that* the system is degraded; traces tell you *why* a specific job
was slow or retried. Add OpenTelemetry: a trace per job, a span per claim/step/
complete, propagated into the LLM call. For a free stack, export to an in-process
collector that logs span events, or to a free Tempo/Jaeger backend. A real
cross-worker trace of a job flowing `claim@worker-1 → steps → kill → reclaim@worker-3
→ resume → complete` is worth more than any red `up` gauge.

**Interview concept:** actual observability depth — the "I'd know this system was
unhealthy in production" answer, with a trace to point at, not just a dashboard.

---

## U9 — Cost-aware rate limiting at the LLM boundary

Textbook rate limiting counts requests per second at the API boundary. **Real
capacity lives at the LLM call boundary and is measured in tokens, not requests.**
Wrap LLM calls with a token-bucket limiter (in-memory ⇒ Upstash Redis distributed)
whose tokens are **estimated token cost per job**, debited against the provider's
actual free-tier budget (e.g., Groq tokens/min). A cheap summarization job and an
expensive codegen job consume different budgets. This is what separates a toy queue
from a system that models the real world.

**Interview concept:** "I limited where the cost actually lives — by estimated token
spend against the provider's real budget — not by request count."

---

## U10 — Deterministic-time simulation testing ✅ **DELIVERED (Week 7)**

Real `time.Sleep` in tests means your suite takes minutes and flakes when CI is
loaded. Inject a `Clock` interface and a `FakeLlm` backend so tests can fast-forward
the clock and **deterministically reproduce** the lease-expiry-while-alive race, the
fencing-token race, and backoff timing — in milliseconds, with no sleeps, no flake.
This is how Temporal and Redis test their own time-dependent correctness.

**Delivered artifacts:**
- `internal/clock/` — `Clock` interface with `Now/After/NewTicker/Sleep`, `SystemClock`, `ManualClock` with min-heap event queue
- `internal/ratelimit/clock.go` — zero-breakage aliases
- `internal/store/` — `PgStore` takes `clock.Clock`, all SQL `now()` → `$now` bind
- `internal/worker/` + `internal/agent/` — virtual timers via injected clock
- `internal/llm/fake.go` — `Script()/Delay()/StepCalls()` deterministic fake
- `internal/sim/` — harness + three named race tests (all green under `-race -count=10`)
- `internal/sim/sim_test.go` — `TestSim_LeaseExpiryWhileAlive`, `TestSim_FencingTokenRace`, `TestSim_BackoffTiming`
- `internal/store/postgres_test.go` — `TestBackoffGatesReclaim`, `TestLeaseExpiryManualClock`

**Verification:**
```bash
go test -race -count=10 ./internal/sim/...   # < 5s, 10× identical output
go test -race -count=5 ./internal/worker/... # chaos + sim, 5× green
go test -race ./internal/store/...            # deterministic backoff/lease tests
```

**Interview concept:** senior testing discipline — the difference between "tests that
sometimes pass" and "tests that prove a property."

---

## How these ripple across the weeks

- **Week 3 (thesis week):** U1, U2, U3, U4, U5 are **core**; U6 and U7 are stretch.
  The crash-recovery demo should be powered by all of U1–U5 together — that is what
  makes the "kill -9 → resume from last step" story *true and provable* rather than
  a lucky recording.
- **Week 4:** the LLM plan→tool→observe loop plugs into the checkpointed step
  scaffold from U4 — each tool call *is* a resumable, fenced step.
- **Week 5:** U9 turns the textbook limiter into a cost-aware one.
- **Week 6:** U8 adds traces alongside the Prometheus dashboard.
- **Week 7:** U10 replaces `time.Sleep` tests with deterministic simulation ✅; U7's
  chaos test runs in CI under `-race`.

---

## The one-sentence story this lets you tell

> "I built a queue where a paused worker can't double-execute a job, a long job
> can't be falsely reclaimed, a crashed job resumes from its last checkpoint, and a
> poison message lands in a dead-letter queue — and I proved the exactly-once-under-
> crash invariant with a deterministic chaos test under the race detector."
