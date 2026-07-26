# Week 3 Demo — `kill -9` → different worker resumes from checkpoint

> The project's thesis, demonstrated live. Run `scripts/kill_recovery_test.sh` and
> this is what happens. This transcript was captured from a real 4-worker Docker
> stack (orchestrator + `worker-1..4` against one Postgres, `WORKER_LEASE=10s`).
> It is the strongest artifact the project produces for an interview.

## What it proves (U1–U5 together)

1. A worker claims a long multi-segment job and **checkpoints segments one at a time** (U4).
2. We `kill -9` (SIGKILL) that worker **mid-job** — no graceful drain, so the job
   row is left `running` with a live-looking lease and a partial checkpoint.
3. After the lease expires, a **different worker reclaims** the row: `running →
   claimed`, **`lease_epoch` increments** (a fresh fencing token — U1), and it
   reads `LastCompletedStep` and **resumes** — recovery, not restart.
4. The job reaches `completed`, and `GET /jobs/{id}/trace` shows every segment
   checkpointed **exactly once** (U1 fencing prevents double-execution by
   construction, not by luck).

## Transcript (real run, 2026-07-24)

```
== submitting 20-segment job via http://localhost:8080 ==
job_id=ef59e419-b63d-46d0-8657-a2292abe68cb
owner (first claim): worker-4
== waiting for a partial checkpoint ==
  checkpointed steps: 0 / 20
  checkpointed steps: 1 / 20
  -> killing mid-job after 1 checkpointed step(s)
== killing owner worker-4 (container forge-worker-4) with kill -9 ==
  killed; lease will expire and a different worker should reclaim
== waiting for reclaim + resume + completion (cap 180s) ==
  status=running claimed_by=worker-4 checkpointed=3/20      <- lease still held (zombie window)
  status=running claimed_by=worker-4 checkpointed=3/20
  status=running claimed_by=worker-4 checkpointed=3/20
  status=running claimed_by=worker-4 checkpointed=3/20
  status=running claimed_by=worker-4 checkpointed=3/20
  status=running claimed_by=worker-2 checkpointed=5/20      <- worker-2 reclaimed, resumed from step 4
  status=running claimed_by=worker-2 checkpointed=6/20
  status=running claimed_by=worker-2 checkpointed=8/20
  status=running claimed_by=worker-2 checkpointed=10/20
  status=running claimed_by=worker-2 checkpointed=13/20
  status=running claimed_by=worker-2 checkpointed=15/20
  status=running claimed_by=worker-2 checkpointed=17/20
  status=running claimed_by=worker-2 checkpointed=19/20
  status=completed claimed_by=worker-2 checkpointed=20/20
owner (final claim): worker-2
PASS: a different worker (worker-2) completed the job, not the killed worker-4
checkpointed steps: total=20 unique=20
PASS: all 20 segments completed exactly once, no gaps
== restarting the killed worker forge-worker-4 to restore the fleet ==
== PASS: kill -9 -> different worker resumed from checkpoint, exactly-once ==
```

Note the "zombie window": for ~5 polls after the kill the row still shows
`worker-4`/`running` because the lease (last renewed every `lease/3`) had not yet
expired. The moment it lapses, the claim query reclaims the `running` row
(`ClaimJob` reclaims `status IN ('claimed','running') AND lease_expires_at <
now()` — U3), bumps `lease_epoch`, and worker-2 resumes. A SIGKILL'd worker-4
could not have written step 4-onward even if it awoke: its fencing token
(epoch 1) no longer matches the row (epoch 2), so its `RecordStep`/`CompleteJob`
return `ErrFenced` and refuse to commit.

## Final job state (`GET /jobs/{id}`)

```json
{
  "id": "ef59e419-b63d-46d0-8657-a2292abe68cb",
  "task_type": "segments",
  "payload": { "segments": 20 },
  "status": "completed",
  "priority": 9,
  "claimed_by": "worker-2",
  "lease_epoch": 2,
  "attempt_count": 2,
  "max_attempts": 3,
  "created_at": "2026-07-24T08:34:28.737732Z",
  "completed_at": "2026-07-24T08:34:53.124368Z",
  "dead_letter": false
}
```

**`lease_epoch=2`** is the proof of the fencing-token mint on reclaim (U1):
worker-4 held epoch 1; its kill left the row at epoch 1; worker-2's claim bumped
it to 2. **`attempt_count=2`** confirms the second claim. `claimed_by=worker-2`
confirms a *different* worker owns it now. Any epoch-1 write would be rejected.

## Trace (`GET /jobs/{id}/trace`) — the money shot

20 contiguous steps, all `completed`, each checkpointed exactly once:

```
20 checkpointed steps:
  step#  status    step_type  duration_ms
     1   completed segment    1126ms
     2   completed segment     803ms
     ...
    20   completed segment    1095ms
min=1 max=20 count=20 unique=20
exactly-once (count==unique): True
contiguous 1..N: True
```

(Steps 1–3 were checkpointed by worker-4 before the SIGKILL; steps 4–20 by
worker-2 after reclaim. The `job_steps` table has no per-step `worker_id`
column, so the split is proven by the `claim`/`epoch`/`attempt_count`
provenance above, not stamped on each step row.)

## How to reproduce

```bash
# 1. Start an isolated stack (postgres + orchestrator + 4 workers). The
#    WORKER_LEASE override makes the reclaim happen ~10s after the kill
#    instead of ~2m, so the demo is fast and observable.
WORKER_LEASE=10s docker compose up -d --build postgres orchestrator \
  worker-1 worker-2 worker-3 worker-4

# 2. Apply migrations into the fresh Postgres (if not already applied):
docker exec forge-postgres psql -U postgres -d forge \
  -f /migrations/000001_initial.up.sql \
  -f /migrations/000002_add_error_message.up.sql \
  -f /migrations/000003_fencing_checkpoints.up.sql

# 3. Run the demo:
WORKER_LEASE=10s API_URL=http://localhost:8080 \
  bash scripts/kill_recovery_test.sh
```

On the Oracle VM over HTTPS:

```bash
WORKER_LEASE=10s API_URL=https://4orge.duckdns.org WORKER_GLOB=forge-worker-? \
  bash scripts/kill_recovery_test.sh
```

---

## U6 capstone — bounded concurrency *under* a crash (2026-07-25)

The demo above runs each worker at `concurrency=1` (serial). Phase 6 (U6) lets a
worker run up to `WORKER_CONCURRENCY` jobs at once behind a bounded semaphore,
**each owning its lease-renewal goroutine + fenced step loop**, all rooted in one
cancellable worker context. The capstone proves concurrency does not weaken the
exactly-once-under-crash invariant: one worker is killed while holding an
in-flight job, and the *same* healthy worker that is running its own job also
reclaims the orphan — concurrently — without double-execution.

Stack: 2 workers × `WORKER_CONCURRENCY=2` (= 4 job slots) against one Postgres,
`WORKER_LEASE=10s`.

```
== submit 2 long jobs (60 segments each, ~36s) ==
job1=93e99c49… claimed_by=worker-2
job2=f664185e… claimed_by=worker-1
== SIGKILL worker-1 (orphans job2) ==
== poll until both completed ==
  J1:completed [worker-2 e=1] steps=60/60   worker-2's own job — never reclaimed
  J2:completed [worker-2 e=2] steps=60/60     worker-2 reclaimed worker-1's orphan (epoch 1->2)
  >>> J2 RECLAIMED by worker-2 (epoch 1->2)
```

worker-2 was running job1 **and** reclaimed job2 concurrently (both fit in its
concurrency=2 budget). Final state of the orphaned job:

```json
{ "status": "completed", "claimed_by": "worker-2",
  "lease_epoch": 2, "attempt_count": 2, "dead_letter": false }
```

Both traces — exactly-once, contiguous, no gaps:

```
J1 (worker-2's own):  steps=60 unique=60  contiguous 1..60  exactly-once
J2 (reclaimed orphan): steps=60 unique=60  contiguous 1..60  exactly-once
```

`lease_epoch=2` on the orphan is the fencing proof: worker-1's zombie, had it
revived, would have written with epoch 1 and been fenced. So bounded
concurrency holds the same U1–U5 guarantees — recovery is resumption, and no
step runs twice even with multiple jobs in flight per worker.

### How to reproduce

```bash
WORKER_LEASE=10s WORKER_CONCURRENCY=2 docker compose up -d --build \
  postgres orchestrator worker-1 worker-2
# Apply migrations (if not already), then submit 2 long jobs and kill one worker:
#   worker-1 takes one, worker-2 the other; kill -9 worker-1 and watch worker-2
#   reclaim its orphan (epoch 1->2) while still running its own job.
```

## U7 — the invariant as a passing test (not just a story)

`internal/worker/chaos_test.go` (`TestChaosRecoveryKillsExactlyOnce`) runs a
fleet of workers against an in-memory store that faithfully emulates fencing +
the SKIP-LOCKED claim + reclaim-`running` + lease expiry, while a seeded
pseudo-random killer cancels a random worker's context (a kill -9 sim) and
respawns a replacement so the fleet stays alive. After the dust settles it
asserts three invariants: **liveness** (every job reaches completed or
dead_letter), **safety** (a per-(job,step) commit counter stays ≤ 1 — no step
executed more than once — and each job's completed steps are exactly {1..K}), and
**no panics / no data races** under `-race`.

A negative control proves the test has teeth: temporarily seeding
`executeJob`'s resume point to 0 (always restart from step 1 instead of
`LastCompletedStep+1`) makes the test fail loudly —
`SAFETY VIOLATION: step … committed N times` (steps re-run up to 15×) — across
every seed, while the correct code passes all seeds.

```bash
# The plan's documented invocation — five repetitions under the race detector:
go test -race -count=5 ./internal/worker/...

# Vary the seed across runs to widen the fuzz surface:
CHAOS_SEED=1234 go test -race -run TestChaosRecoveryKillsExactlyOnce ./internal/worker/...
```

