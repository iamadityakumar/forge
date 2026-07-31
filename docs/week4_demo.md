# Week 4 Demo — `kill -9` → different worker resumes the agent loop

> Week 3 proved crash recovery for fixed dummy segments. Week 4 proves the same
> recovery story for a real LLM agent loop: every LLM decision and every tool
> execution is a durable, fenced checkpoint row.

## What it proves

1. A `cp_solve` job is claimed by one worker and begins a real
   **plan → tool_call → observe** loop.
2. The worker commits each LLM decision as a `plan` row before executing the tool,
   then commits the tool observation as a `tool_call` row.
3. We `kill -9` the owning worker mid-loop. The job remains `running` until the
   lease expires.
4. A **different worker** reclaims the job, bumps `lease_epoch`, rebuilds the LLM
   conversation from `job_steps`, and resumes from the last committed checkpoint.
5. The final trace has contiguous step numbers, no duplicates, both `plan` and
   `tool_call` rows, and at least two distinct `worker_id` values.

The strongest case is when the kill lands after a `plan` row but before its
matching `tool_call`: the reclaiming worker's first committed row is `tool_call`,
not `plan`. That proves the expensive LLM decision was recovered from Postgres and
not recomputed — zero LLM re-spend for the committed decision.

## Protocol

Each agent iteration uses two durable rows:

| Step | Type | Meaning |
|---|---|---|
| `2k-1` | `plan` | The raw LLM decision JSON (`action:"tool"` or `action:"finish"`) |
| `2k` | `tool_call` | The tool output / observation for that decision |

Recovery reads `GET /jobs/{id}/trace` / `store.ListSteps`, sorts by
`step_number`, and reconstructs the conversation:

- `plan` rows become assistant messages.
- `tool_call` rows become user observation messages.
- A trailing lone `plan` with `action:"tool"` is a pending committed decision;
  the new worker runs the tool without calling the LLM again.
- A trailing `plan` with `action:"finish"` means the job was logically complete;
  the worker returns nil and the worker loop transitions the job to `completed`.

This is still honest about the unavoidable edge case: if the worker is killed
while the LLM HTTP call is in flight and **before** a `plan` row commits, there is
no durable decision to reuse. The reclaiming worker calls the LLM again. The
zero-re-spend guarantee applies once the `plan` row has committed.

## How to reproduce

### Local stack

```bash
# Start the stack with a short lease so reclaim is fast.
WORKER_LEASE=10s docker compose up -d --build postgres orchestrator \
  worker-1 worker-2 worker-3 worker-4

# Apply migrations if this is a fresh database.
docker exec forge-postgres psql -U postgres -d forge \
  -f /migrations/000001_initial.up.sql \
  -f /migrations/000002_add_error_message.up.sql \
  -f /migrations/000003_fencing_checkpoints.up.sql \
  -f /migrations/000004_step_worker_attribution.up.sql

# The worker image must contain python3 for the run_tests tool.
docker exec forge-worker-1 python3 --version

# The agent also needs a configured LLM backend. Ollama is the default;
# for containers on Docker Desktop, compose defaults OLLAMA_HOST to
# http://host.docker.internal:11434. For Groq, set LLM_BACKEND=groq and
# GROQ_API_KEY in .env, then recreate workers.

API_URL=http://localhost:8080 bash scripts/cp_solve_agent_demo.sh
```

### Oracle VM over HTTPS

```bash
ssh ubuntu@4orge.duckdns.org -i ~/.ssh/forge_vm
cd ~/forge

git pull origin main
for f in migrations/*.up.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$f"; done

docker compose build worker orchestrator
docker compose up -d

docker compose exec worker-1 sh -c 'python3 --version'

# Use the VM's configured backend (Groq recommended for a reliable live demo), then:
bash scripts/cp_solve_agent_demo.sh
```

## Expected transcript shape

A successful run looks like this:

```text
== submitting a cp_solve agent job via https://4orge.duckdns.org ==
job_id=<uuid>
owner (first claim): worker-3
== waiting for the agent to commit its first plan (cap 60s) ==
  first plan committed (1 committed step(s))
== watching for the mid-iteration kill window (cap 60s) ==
  committed steps: 1
  committed steps: 2
  mid-iteration window caught: 3 committed steps, last row is a lone plan
kill mode: mid_iteration
== killing owner worker-3 (container forge-worker-3) with kill -9 ==
  killed; lease will expire and a different worker should reclaim
== waiting for reclaim + resume + completion (cap 300s) ==
  status=running claimed_by=worker-3 committed=3
  ...
  status=running claimed_by=worker-1 committed=4
  ...
  status=completed claimed_by=worker-1 committed=5
owner (final claim): worker-1
PASS: a different worker (worker-1) completed the job, not the killed worker-3
steps=5 types=plan,tool_call worker_ids=worker-1,worker-3 worker_boundaries=[3]
MONEY SHOT: reclaimer's first committed step is a tool_call — the checkpointed LLM decision was reused, not recomputed
PASS: all 5 plan/tool_call steps committed exactly once, contiguous; two-worker trace; finish decision durable
== restarting the killed worker forge-worker-3 to restore the fleet ==
== PASS: kill -9 -> different worker resumed the agent from checkpoint, exactly-once, two-worker trace ==
```

If the kill lands between full iterations rather than between a `plan` and its
`tool_call`, the demo still proves cross-worker crash recovery and exactly-once
checkpoint commits. It may not print the `MONEY SHOT` line because the first
reclaimer step can legitimately be a new `plan`.

## Trace assertions made by the script

`scripts/cp_solve_agent_demo.sh` fails fast unless all of these hold:

- final job status is `completed`, not `failed`;
- final `claimed_by` is different from the killed owner;
- step numbers are exactly `1..N` with no duplicates or gaps;
- every step status is `completed`;
- the trace contains both `plan` and `tool_call` rows;
- the trace contains at least two distinct non-empty `worker_id` values;
- the final `plan` row parses as a decision with `action:"finish"`.

## Captured live evidence

_To be filled after Task 7.3 is run on the Oracle VM._

```text
DEMO_EXIT=<pending>
job_id=<pending>
killed_worker=<pending>
reclaimer=<pending>
trace_workers=<pending>
```
