# Week 3 — Multi-Worker & Crash Recovery (the project's thesis week)

> **The thesis.** `kill -9` a worker mid-job → a *different* worker reclaims the job → it
> resumes from the last *checkpointed step* → **zero steps executed twice.** This is the one
> story the whole project exists to make true and provable.
>
> This week is driven by `STANDOUT_UPGRADES.md` — the lens that turns the textbook
> "queue + `SKIP LOCKED`" into something defensible at a senior interview. Every task below is
> tagged with the upgrade it implements (**U1–U7**) and the interview concept it embodies.

---

## 0. Where we are now (progress snapshot)

| Area | Status | Evidence |
|------|--------|----------|
| **Week 1** — API skeleton, schema, Oracle VM deploy, HTTPS | ✅ done & committed | `cmd/orchestrator`, `migrations/000001`, Caddy/DuckDNS commits (`37aa360`+). *Note: the checkbox table in `week1_plan.md` still marks DuckDNS/Caddy (6a–6d) ☐ — stale; the work landed in git.* |
| **Week 2** — `SKIP LOCKED` claim, state machine, worker binary, `/jobs`, `/health`, full create schema, drain test | ✅ done & committed | `aa2560a` "wire Postgres store, add worker binary, SKIP LOCKED claim query"; tests `ca8429c`/`0bb58f0`; `scripts/drain_test.sh`. |
| **Week 3** — fencing, checkpoints, lease extension, retry/DLQ, multi-worker, chaos test | ✅ **Phases 1–7 done** & committed (`ca24929`–`44e7d70`, merged to `main` as `cabe73b` via PR #1); live HTTPS demo PASS | U1–U7 wired, committed & CI-green; the crash-recovery thesis was demonstrated **live over `https://4orge.duckdns.org`** (2026-07-28, `DEMO_EXIT=0`: `kill -9 worker-4` mid-job → `worker-3` reclaimed `lease_epoch 1→2` & resumed from last checkpoint, 20/20 segments exactly-once) — see [`docs/week3_demo.md`](docs/week3_demo.md). |

### Week 3 — done / in-progress / not started

✅ **Done (uncommitted, in the working tree — `git status`):**
- `STANDOUT_UPGRADES.md` — the U1–U10 design lens. *(the plan you are reading is its execution doc)*
- `migrations/000003_fencing_checkpoints.{up,down}.sql` — adds `lease_epoch`, `run_at`, `completed_at`, `dead_letter`; replaces `idx_jobs_claimable` with two **partial indexes** (`idx_jobs_pending`, `idx_jobs_expired_lease`). → *schema half of U1/U3/U5 + the partial-index touch.*
- `internal/store/models.go` — `Job` gained the 4 Week-3 fields; `JobStep` struct + step status constants defined.
- `internal/store/store.go` — `ErrFenced` sentinel error added.

🔴 **Not started — the actual behavior (verified absent by reading `postgres.go`, `worker/loop.go`, `api/`):**

| Upgrade | What the scaffolding *prepared* | What is still missing |
|---|---|---|
| **U1 fencing tokens** | `lease_epoch` column exists | `ClaimJob` does **not** `lease_epoch = lease_epoch + 1`, does **not** return it; `StartJob`/`CompleteJob`/`FailJob` do **not** filter `AND lease_epoch = $epoch`; `JobStore` interface methods carry **no** epoch param. |
| **U3 reclaim `running`** | `idx_jobs_expired_lease` predicate already covers `('claimed','running')` | The claim subselect in `postgres.go:174` **still only reclaims `'pending' ∪ ('claimed' ∧ expired)`** — `'running'` is NOT reclaimed. The Week-3 demo-killer bug is still live in code. |
| **U4 checkpoints** | `JobStep` model + `job_steps` table | **No** `RecordStep` / `LastCompletedStep` store methods; `executeJob` is still the Week-2 opaque sleep (no multi-step checkpoint loop); **no** `GET /jobs/{id}/trace`. |
| **U2 lease extension** | — (nothing) | No `RenewLease` method; `worker.Run` has **no** per-job renewal goroutine; lease is still a fixed `2*time.Minute`; no zombie self-abort on `ErrFenced`. |
| **U5 retry + DLQ** | `run_at`, `dead_letter`, `attempt_count`/`max_attempts` columns | `FailJob` still sets `status='failed'` **permanently** (no `attempt_count < max_attempts` requeue branch, no `run_at = now()+backoff`, no dead-letter). Claim subselect lacks the `AND run_at <= now()` gate. |
| **U6 bounded concurrency** (stretch) | — | `worker.Run` is still **serial** one-job-at-a-time. |
| **U7 chaos/invariant test** (stretch) | — | None. *(And the prior Week-2 test that proved concurrent claiming has been deleted — see §1.0.)* |

🟡 **In progress / regression to resolve before coding:**
- `git status` shows **`D internal/store/postgres_test.go`** — the committed Week-2 integration tests (concurrent-claim test = task 2.5, state-transition tests = task 2.6) have been **deleted from the working tree**. Those tests must be **restored and rewritten** around the new fenced signatures (Phase 0), otherwise Week 3 builds on a test regression.
- Same deletion applies to `scripts/test_payload.json` and `scripts/verify_api.sh` (dev helpers, low priority).
- The `RETURNING`/`SELECT` column lists + `Scan(...)` in `postgres.go` do not yet reference the 4 new columns, so `CreateJob`/`GetJob`/`ListJobs`/`ClaimJob` compile but leave the Week-3 fields **zero-valued**. This gets fixed naturally in Phase 1.

**Bottom line:** the migration and types have been staged, but the moment you make U1 real you must rewrite the `JobStore` interface, every claim/transition query in `postgres.go`, and the worker loop — and that rewrite is the bulk of the week.

---

## Time & scope

**Budget:** ~10–12 focused hours. **Core (must finish):** U1, U3, U4, U2, U5 + the demo. **Stretch (nice, not blocking):** U6 bounded concurrency, U7 chaos test.

**Hard order dependency:** U1 (fencing) is the foundation — every other upgrade is a fenced write on top of it. Do Phase 1 first.

---

## Phase 0 — Restore the test baseline

### Task 0.1 — Recover the deleted Week-2 tests
`git checkout HEAD -- internal/store/postgres_test.go` (and the two dev scripts if you want them).
Then stub the build: the restored tests call `ClaimJob(ctx, workerID, lease)` / `StartJob(ctx, id)` — the **old** signatures. They will not compile once Phase 1 changes the interface. Keep them as the reference you restore each test against in Phase 6.

**Acceptance:** `postgres_test.go` is back on disk (compiles against the *current* interface — it's allowed to break after Phase 1, that's expected).

---

## Phase 1 — Fencing tokens + reclaim `running` (U1, U3) *— the foundation*

> Everything that mutates a job now carries an **epoch** (fencing token) and fails atomically
> (`0 rows ⇒ ErrFenced`) if the caller was deposed. This is Kleppmann fencing tokens: double
> execution is prevented *by construction*, not by luck.

### Task 1.1 — Extend the `JobStore` interface with epoch-threaded methods
**File:** `internal/store/store.go`

```go
// ClaimJob atomically claims the next available job and returns the NEW lease_epoch
// the caller must use as a fencing token for every subsequent fenced write.
ClaimJob(...) (*Job, error)            // unchanged signature, but now returns job.LeaseEpoch = n+1

StartJob(ctx, jobID uuid.UUID, epoch int) error       // treated as fencing token
CompleteJob(ctx, jobID uuid.UUID, epoch int) error
FailJob(ctx, jobID uuid.UUID, epoch int, reason string) error   // see Phase 4 requeue branch
RenewLease(ctx, jobID uuid.UUID, epoch int, lease time.Duration) error  // Phase 3
RecordStep(ctx, jobID uuid.UUID, epoch int, step JobStep) (int, error) // Phase 2
LastCompletedStep(ctx, jobID uuid.UUID) (int, error)                   // Phase 2
ListSteps(ctx, jobID uuid.UUID) ([]JobStep, error)                     // Phase 5 (trace API)
```

**Acceptance:** Interface compiles; the orchestrator's calls update to pass `0`/no epoch where the API doesn't own a lease.

### Task 1.2 — Make `ClaimJob` increment & return epoch, reclaim `running`, gate `run_at`
**File:** `internal/store/postgres.go`

```sql
UPDATE jobs
SET status           = 'claimed',
    claimed_by       = $1,
    lease_expires_at = now() + $2::interval,
    lease_epoch      = lease_epoch + 1,   -- U1: mint a new fencing token
    attempt_count    = attempt_count + 1
WHERE id = (
    SELECT id FROM jobs
    WHERE status = 'pending'
       OR (status IN ('claimed','running') AND lease_expires_at < now())  -- U3: reclaim running too
    ORDER BY priority DESC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING ..., lease_epoch, run_at, completed_at, dead_letter;   -- multiple-column RETURNING list
```
- **U3 fix** (the original Week-3 bug): the subselect now reclaims `running` too — the state a worker is left in *after* `StartJob` when it crashes. The `idx_jobs_expired_lease` partial index already covers it.
- Gate scheduled jobs: add `AND (run_at IS NULL OR run_at <= now())` to the subselect's `WHERE` (U5, enforces backoff delay on requeue).
- Extend every `RETURNING`/`SELECT`/`Scan` (Create/Get/List/Claim) to include `lease_epoch, run_at, completed_at, dead_letter` so the new fields actually populate.

**Acceptance:** A reclaimed expired-`running` job comes back `claimed` with `lease_epoch` incremented by 1. A `run_at` in the future is not claimed until it elapses.

### Task 1.3 — Fence every state transition
**File:** `internal/store/postgres.go` — `StartJob`, `CompleteJob`, `FailJob`

```sql
UPDATE jobs SET status='running'  WHERE id=$1 AND status='claimed'  AND lease_epoch=$2;  -- returns 0 → ErrFenced
UPDATE jobs SET status='completed', completed_at=now()
               WHERE id=$1 AND status='running' AND lease_epoch=$2;
-- FailJob branches in Phase 4 (Task 4.1).
```
`0 rows affected` with a *matching* status but *mismatched* epoch ⇒ `ErrFenced`; status mismatch ⇒ `ErrInvalidTransition` (keep distinguishing them — the worker's reaction differs).

**Acceptance:** A "zombie" (old epoch) `CompleteJob` returns `ErrFenced`, not `ErrInvalidTransition`; a live holder's call still succeeds.

---

## Phase 2 — Checkpointed, resumable, idempotent steps (U4)

> A job becomes a sequence of **K short segments**, each checkpointed. On reclaim the new worker
> reads `LastCompletedStep` and starts at `+1`. This is WAL/replay: recovery is *resumption*,
> not *restart.* Week 4 later swaps the segment body for a real plan→tool→observe step.

### Task 2.1 — `RecordStep` (fenced CTE, idempotent upsert)
**File:** `internal/store/postgres.go`

```sql
WITH owned AS (
    SELECT 1 FROM jobs WHERE id = $1 AND lease_epoch = $2 FOR UPDATE  -- U1 fence
)
INSERT INTO job_steps (job_id, step_number, step_type, input, output, status, duration_ms)
SELECT $1, $3, $4, $5, $6, 'completed', $7 FROM owned
ON CONFLICT (job_id, step_number) DO UPDATE
SET output=EXCLUDED.output, status=EXCLUDED.status, duration_ms=EXCLUDED.duration_ms
RETURNING id;
```
Returns `(id, 0-rows-⇒-ErrFenced)`.

### Task 2.2 — `LastCompletedStep`
`SELECT COALESCE(MAX(step_number),0) FROM job_steps WHERE job_id=$1 AND status='completed';`

### Task 2.3 — Replace `executeJob` with a checkpointed multi-step loop
**File:** `internal/worker/execute.go` *(split out from `loop.go`)*

```go
func executeJob(ctx, s store.JobStore, job *store.Job, epoch int) error {
    start, _ := s.LastCompletedStep(ctx, job.ID)   // resume point
    steps := decodeSegmentCount(job.Payload)        // K from payload (default 5)
    for i := start + 1; i <= steps; i++ {
        out, err := runSegment(ctx, job, i)         // dummy work for Week 3
        if err != nil { return err }
        if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
            JobID: job.ID, StepNumber: i, StepType: "segment", Output: out,
        }); err != nil {
            if errors.Is(err, store.ErrFenced) { return err }  // I was deposed → abandon
            return err
        }
    }
    return nil
}
```
`runSegment` does a tiny bounded sleep per segment (so a kill mid-step leaves the job `running`
with only the *completed* segments checkpointed — the demo's failure path).

**Acceptance:** Submit a 5-segment job, let it finish, `GET /jobs/{id}/trace`-equivalent shows steps 1–5 `completed` exactly once.

---

## Phase 3 — Lease extension as the alive-signal (U2)

> A renewal *is* a heartbeat that also encodes ownership, and it's self-fencing: a zombie's
> renewal returns 0 rows and tells the executor to abort. Collapses "worker presence" and
> "job ownership" into one mechanism (Temporal heartbeats / Kafka `max.poll.interval.ms`).

### Task 3.1 — `RenewLease` (fenced)
```sql
UPDATE jobs SET lease_expires_at = now() + $2::interval
WHERE id=$1 AND lease_epoch=$3 AND status IN ('claimed','running');
-- 0 rows ⇒ ErrFenced ⇒ worker aborts the in-flight job
```

### Task 3.2 — Per-job lease-extension goroutine in `worker.Run`
**File:** `internal/worker/loop.go` — for each claimed job, spawn a goroutine that renews every
`lease/3`, fenced by epoch. On `ErrFenced`, cancel the job's context so `executeJob`/`runSegment`
returns immediately (no wasted work, no continued side effects). Tie the goroutine's lifecycle to
the job's context (structured concurrency) — it must exit before `Run` returns the job's result.

**Acceptance:** A job that runs longer than the lease window completes (no false reclaim) while
its *-worker* is healthy. A worker whose lease can't renew (deposed) aborts within `lease/3`.

---

## Phase 4 — Retry with backoff + jitter + dead-letter (U5)

### Task 4.1 — `FailJob` branches: requeue vs. dead-letter
**File:** `internal/store/postgres.go` (fenced, epoch-threaded)

```sql
-- attempt_count < max_attempts  ⇒ requeue for a scheduled retry
UPDATE jobs SET status='pending', claimed_by=NULL, lease_expires_at=NULL,
                run_at = now() + ($3::text)::interval, error_message=$4, lease_epoch=lease_epoch+1
WHERE id=$1 AND lease_epoch=$2 AND attempt_count < max_attempts;
-- else ⇒ terminal dead-letter
UPDATE jobs SET status='failed', dead_letter=true, error_message=$4, completed_at=now()
WHERE id=$1 AND lease_epoch=$2 AND attempt_count >= max_attempts;
```
Backoff = `base * 2^(attempt-1)` capped at 5m, **+ jitter** (avoids thundering-herd retry
storms). Use a `Clock` seam if you want this deterministic in tests (U10 is Week 7 — a thin
`func() time.Time` injectable is enough for now).

**Acceptance:** A job that fails 3× (max) lands in `dead_letter=true, status='failed'`, never
reclaimed. A job failing under the limit requeues with `run_at` in the future and is *not* claimed
until it elapses (proved by the Phase-1 `run_at` gate).

### Task 4.2 — Surface dead-letter jobs
`GET /jobs?status=dead_letter` — either teach `ListJobs` to map `dead_letter` to a
`status='failed' AND dead_letter=true` filter, or add `GET /jobs?dead_letter=true`. Pick one and
document it.

---

## Phase 5 — Multi-worker + trace API + the demo

> **Status: ✅ DONE & demonstrated live (2026-07-24).** All three tasks landed and
> the crash-recovery demo ran successfully *twice* against a real 4-worker Docker
> stack (`WORKER_LEASE=10s`): worker-4 `kill -9` → worker-2 reclaimed
> (`lease_epoch 1→2`) & resumed; then worker-1 `kill -9` → worker-3 reclaimed.
> Both runs: job reached `completed`, **20/20 segments checkpointed exactly once
>**, no gaps. Full transcript + final job/trace evidence in
> [`docs/week3_demo.md`](docs/week3_demo.md). The throwaway demo stack was torn
> down afterward; the host's existing `forge-postgres` was left untouched.

### Task 5.1 — Run 3–5 workers concurrently
`docker-compose.yml`: add `deploy.replicas: 4` (or explicit `worker-1..worker-4` services) each
with a distinct `WORKER_ID`. The polling loop already supports N concurrent workers against one
store — verify no duplicate claims under load (= the Phase 6.1 test).

### Task 5.2 — `GET /jobs/{id}/trace`
**Files:** `internal/store/postgres.go` (`ListSteps`), `internal/api/handlers.go` + `routes.go`.
Returns ordered `job_steps` for a job — the **demo's money shot**: watch the step timeline fill in
live as worker B resumes worker A's checkpoint after the kill.

```go
r.Get("/jobs/{id}/trace", h.jobTraceHandler)
```

### Task 5.3 — The crash-recovery script (the thesis demonstration)
**File:** `scripts/kill_recovery_test.sh` — submit a long multi-segment job, poll `/trace`,
`kill -9` the owning worker mid-step, assert: a *different* `claimed_by` appears, the job reaches
`completed`, and **no step number appears twice** with `status='completed'`.

**Acceptance:** Script exits 0; manual `kill -9`→resume observed; record the GIF/screencap for the
README (this is the strongest interview artifact the project produces).

---

## Phase 6 — Stretch: bounded concurrency (U6) + invariant chaos test (U7)

> **Status: ✅ DONE & demonstrated (2026-07-25).** Both stretches landed. **U6:**
> `worker.Run` now runs up to `WORKER_CONCURRENCY` jobs at once behind
> `golang.org/x/sync/semaphore` (each job owns its lease + fenced step loop,
> rooted in the worker's cancellable context; structured concurrency drains all
> in-flight work on shutdown). `WORKER_CONCURRENCY` is wired in `cmd/worker` and
> the compose workers (default 1 = serial baseline). **U7:**
> `internal/worker/chaos_test.go` runs a seeded-random killer against an in-memory
> store emulating fencing + SKIP LOCKED + reclaim-running, asserting liveness +
> exactly-once + no races; passes `go test -race -count=5` (all seeds) and its
> **negative control** fails loudly when resume-from-checkpoint is broken
> (`SAFETY VIOLATION: step … committed N times`). A live U6 capstone killed one
> worker while 2 were in flight; the healthy worker reclaimed the orphan
> (`lease_epoch 1→2`) **concurrently** with its own job — both 60-segment traces
> exactly-once. See [`docs/week3_demo.md`](docs/week3_demo.md) §U6/U7.

### Task 6.1 — Bounded per-worker concurrency (U6)
`worker.Run` runs up to `WORKER_CONCURRENCY` jobs behind
`golang.org/x/sync/semaphore`, each owning its lease goroutine + fenced step loop, all rooted in
one cancellable context. **Structured concurrency:** on shutdown every in-flight job + lease goroutine
tears down cleanly.

### Task 6.2 — Invariant-based chaos test under `-race` (U7)
**File:** `internal/worker/chaos_test.go` — seeded pseudo-random killer cancels workers mid-step;
assert three **invariants**:
1. **Liveness:** every job reaches `completed` or `dead_letter` (none stuck forever).
2. **Safety (the headline):** **no step is executed more than once** — a per-(job,step) execution
   counter in a fake tool stays ≤1 (exactly-once-under-crash).
3. **No panics / no data races** under `go test -race`.

Run with `go test -race -count=5 ./internal/worker/...`. This is the project's thesis expressed as
a passing test instead of a story — the interview material you cannot fake.

---

## Phase 7 — Restore & extend tests, then redeploy

> **Status (2026-07-28).** (7.1/7.2 unchanged since 2026-07-27; 7.3 landed 2026-07-28.)
> **7.1 — ✅:** the recovered Week-2 store integration tests
> (`internal/store/postgres_test.go`) assert the fenced invariants — epoch
> increments on claim, `running` reclaim, `ErrFenced` on a deposed worker's stale
> `CompleteJob`, and the requeue-vs-dead-letter branch — and pass
> `go test -race ./...` against a real Postgres (verified end-to-end against an
> isolated throwaway stack with schema migrations 1→2→3 applied; exit 0). They
> compile against the fenced `JobStore` because the Phase-1/Phase-4 commits
> (`ca24929`/`886affc`) landed them, so this task was folded into those phases.
> **7.2 — ✅:** `.github/workflows/ci.yml` runs on `push` + `pull_request`,
> spins a `postgres:15` service, applies `migrations/*.up.sql` in order with
> `psql`, then runs `go vet` / `go build` / `go test -race ./...` plus
> `go test -race -count=5 ./internal/worker/...` (the U7 chaos fuzz, count>1 to
> defeat the result cache). actionlint-clean; the chaos step passes locally
> (~45s).
> **7.3 — ✅ (production deploy; run on the Oracle VM):** the Week-3 work is
> merged to `main` as commit `cabe73b` (PR #1, 2026-07-27) — `git diff main
> origin/worktree-week3-plan` is empty, so building `main` *is* the Week-3 code.
> On the VM (`~/forge`, `main` @ `cabe73b`): migration `000003` was already
> applied (`lease_epoch`/`run_at`/`completed_at`/`dead_letter` + `job_steps` all
> present), so the re-apply guard was a no-op; the stack (already) runs with
> `WORKER_LEASE=10s`, `WORKER_CONCURRENCY=1`, 4 workers, behind Caddy HTTPS at
> `4orge.duckdns.org`. The crash-recovery demo was then run **over HTTPS against
> `https://4orge.duckdns.org`** (2026-07-28, `DEMO_EXIT=0`): a 20-segment job was
> claimed by `worker-4`, which checkpointed **1** segment and was `kill -9`'d
> mid-job (no graceful drain). After the ~10s lease expiry a **different**
> worker, `worker-3`, reclaimed it (`lease_epoch 1→2`, `attempt_count 1→2`,
> `running→claimed`), read `LastCompletedStep`, resumed **from step 1** (steps
> 1→3→4→…→20, never re-doing segment 1), and reached `completed`. `GET
> /jobs/{id}/trace` returned **20 steps, all `status='completed'`,
> `DISTINCT(step_number)=COUNT=20`** (exactly-once, no gaps) by two different
> workers; the script printed the acceptance line `PASS: kill -9 -> different
> worker resumed from checkpoint, exactly-once` and exited 0, then restarted
> `worker-4` — fleet back to 4 healthy workers, restart policy restored to
> `unless-stopped`. The replayable runbook remains appended to Task 7.3 below.

### Task 7.1 — Rewrite the recovered Week-2 tests against fenced signatures
`internal/store/postgres_test.go`: concurrent-claim (task 2.5) + state-transition (task 2.6), now
asserting epoch increments, `running` reclaim, `ErrFenced` on zombie writes, requeue-vs-DLQ branch.

### Task 7.2 — Wire CI (ahead of Week 7, because the chaos test wants it)
`.github/workflows/ci.yml` — `go test -race ./...` on push against a service Postgres. The
`-race` detector is precisely what catches the subtle claiming bugs Week 3 introduces; running
it in CI has outsized signal.

### Task 7.3 — Reapply migration `000003` + redeploy to the Oracle VM
`docker compose up -d --build`, run `scripts/kill_recovery_test.sh` against `https://4orge.duckdns.org`,
verify recovery over HTTPS.

> **Run this on the Oracle VM** (it rebuilds the live stack and `kill -9`s a
> worker container over HTTPS — an outward-facing action, so it is run from the
> VM, not from the dev worktree):
>
> ```bash
> # 1. Get this branch on the VM (or however the VM pulls the code).
> git checkout worktree-week3-plan && git pull
>
> # 2. migration 000003 is already applied on the VM's forge DB (commit 6d39fc0
> #    migrated it). Re-apply ONLY if a Week-3 column is absent (no-op otherwise):
> docker exec forge-postgres psql -U postgres -d forge -tAc \
>   "SELECT column_name FROM information_schema.columns
>    WHERE table_name='jobs' AND column_name='lease_epoch';" \
>   | grep -q lease_epoch || psql "$DATABASE_URL" -f migrations/000003_fencing_checkpoints.up.sql
>
> # 3. Rebuild + redeploy with a SHORT per-job lease so reclaim-then-resume
> #    happens ~10s after the kill instead of ~2m. WORKER_LEASE is read by
> #    docker-compose.yml; the workers must (re)start with it.
> WORKER_LEASE=10s docker compose up -d --build
>
> # 4. The thesis demo over HTTPS: submit a long multi-segment job, kill -9 the
> #    owning worker mid-segment, and assert a DIFFERENT worker reclaims+resumes
> #    with every segment checkpointed exactly once.
> API_URL=https://4orge.duckdns.org WORKER_GLOB=forge-worker-? \
>   bash scripts/kill_recovery_test.sh
> ```
>
> Acceptance = the script prints `PASS: kill -9 -> different worker resumed from
> checkpoint, exactly-once` and exits 0.

---

## Progress Tracking Table

> Order matters: Phase 1 (fencing) gates everything. Mark ☑ as you land each.

| # | Task | Upgrade | Core/Stretch | Status |
|---|------|---------|--------------|--------|
| 0.1 | Restore deleted `postgres_test.go` | — | core | ☑ |
| 1.1 | `JobStore` interface gains epoch-threaded methods | U1 | core | ☑ |
| 1.2 | `ClaimJob`: increment+return epoch, reclaim `running`, gate `run_at` | U1,U3,U5 | core | ☑ |
| 1.3 | Fence `StartJob`/`CompleteJob` (`AND lease_epoch=$epoch → ErrFenced`) | U1 | core | ☑ |
| 2.1 | `RecordStep` fenced idempotent CTE upsert | U1,U4 | core | ☑ |
| 2.2 | `LastCompletedStep` (`MAX(step_number)`) | U4 | core | ☑ |
| 2.3 | Checkpointed multi-segment `executeJob` + resume-from-last | U4 | core | ☑ |
| 3.1 | `RenewLease` fenced write | U2 | core | ☑ |
| 3.2 | Per-job lease-extension goroutine + abandon-on-`ErrFenced` | U2 | core | ☑ |
| 4.1 | `FailJob` branch: requeue (backoff+jitter) vs dead-letter | U5 | core | ☑ |
| 4.2 | Dead-letter surfacing (`GET /jobs?status=dead_letter`) | U5 | core | ☑ |
| 5.1 | Run 3–5 worker replicas in `docker-compose` | — | core | ☑ |
| 5.2 | `GET /jobs/{id}/trace` (step timeline) | U4 | core | ☑ |
| 5.3 | `scripts/kill_recovery_test.sh` + screencap the demo | U1–U5 | core | ☑ |
| 6.1 | Bounded per-worker concurrency (`semaphore`) | U6 | stretch | ☑ |
| 6.2 | Invariant chaos test under `-race` (incl. negative control) | U7 | stretch | ☑ |
| 7.1 | Rewrite Week-2 tests against fenced signatures | — | core | ☑ |
| 7.2 | CI workflow (`go test -race` + Postgres service) | — | core | ☑ |
| 7.3 | Migrate + redeploy to VM, run recovery over HTTPS | — | core | ☑ |

---

## Week 3 Checkpoint (must be demo-able)

> A long multi-segment job is running on worker A. `kill -9` A mid-step. Worker B reclaims (epoch
> increments, `running→claimed`), reads `LastCompletedStep`, resumes the remaining segments, and
> completes. `GET /jobs/{id}/trace` shows every segment executed **exactly once**, by two different
> workers, with zero gaps and zero duplicates — over `https://4orge.duckdns.org`. A poison-message
> job lands in `dead_letter` after `max_attempts`.
>
> **✅ Demonstrated live over HTTPS (2026-07-28, `DEMO_EXIT=0`):** `worker-4` was
> `kill -9`'d mid-job after **1/20** segments → a **different** worker,
> `worker-3`, reclaimed it (`lease_epoch 1→2`, `attempt_count 1→2`,
> `running→claimed`), read `LastCompletedStep`, resumed → `completed`. `trace`
> shows **20 steps `completed`** with `DISTINCT(step_number)=COUNT=20`
> (exactly-once, no gaps) by `worker-4` then `worker-3`; the killed worker was
> restarted, fleet restored to 4 healthy. Script printed `PASS: kill -9 ->
> different worker resumed from checkpoint, exactly-once`.

---

## What comes next (Week 4 preview)

- `LLMBackend` interface (`Complete(ctx, prompt)`) + `ollama.go` / `groq.go`.
- Plan → tool call → observe loop plugs into the **U4 checkpoint scaffold** from this week — each
  tool call *is* a resumable, fenced step. No queue changes; the agent becomes the step body.
- 2–3 real tools behind a sandboxed `exec`; a real multi-step agent trace, checkpointed & recoverable.
