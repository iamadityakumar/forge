# Deploy Forge to Oracle Cloud ARM VM

This runbook documents the exact steps to deploy and verify Forge on the Oracle Cloud Always Free ARM VM (`4orge.duckdns.org`). It reflects the live deployment executed on **2026-08-07**.

---

## Prerequisites

- SSH key: `~/.ssh/forge_vm` (private) / `~/.ssh/forge_vm.pub` (public)
- VM public hostname: `4orge.duckdns.org` (user: `ubuntu`)
- Repo on VM: `~/forge` (cloned from `https://github.com/iamadityakumar/forge.git`, branch `main`)
- Docker + Docker Compose installed on VM
- **Groq API key** for LLM backend (required for `cp_solve` agent jobs)

---

## 1. Verify VM State (read-only)

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  cd ~/forge &&
  echo "=== git ===" && git status -sb && git log --oneline -1 &&
  echo "=== containers ===" && docker compose ps &&
  echo "=== .env LLM ===" && grep -E "^(LLM_BACKEND|GROQ_MODEL)=" .env
'
```

Expected: `main` at `origin/main` HEAD, 7 containers (postgres, orchestrator, 4 workers, caddy) all `Up`, `LLM_BACKEND=groq` after setup.

---

## 2. Configure Groq LLM Backend (one-time)

The VM must have a valid `GROQ_API_KEY` in `~/forge/.env` for `cp_solve` jobs.

```bash
# On your local machine, export the key, then run:
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org "
  cd ~/forge &&
  cp .env .env.bak.\$(date +%Y%m%d%H%M%S) &&
  sed -i 's/^LLM_BACKEND=.*/LLM_BACKEND=groq/' .env &&
  sed -i '/^GROQ_API_KEY=/d' .env &&
  echo 'GROQ_API_KEY=gsk_YOUR_KEY_HERE' >> .env &&
  sed -i 's/^GROQ_MODEL=.*/GROQ_MODEL=llama-3.1-8b-instant/' .env &&
  sed -i 's|^GROQ_BASE_URL=.*|GROQ_BASE_URL=https://api.groq.com/openai/v1|' .env &&
  grep -E '^(LLM_BACKEND|GROQ_MODEL|GROQ_BASE_URL)=' .env
"
```

> **Security:** Do not commit the key. The `.env` file is git-ignored.

---

## 3. Rebuild & Recreate Workers

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  cd ~/forge &&
  docker compose build orchestrator worker-1 worker-2 worker-3 worker-4 &&
  docker compose up -d --force-recreate worker-1 worker-2 worker-3 worker-4 &&
  docker compose ps
'
```

**Note:** The service names are `worker-1` through `worker-4` (not `worker`). The stale command `docker compose build worker orchestrator` fails with `no such service: worker`.

---

## 4. Smoke Test the API

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  curl -sS -m 10 -X POST http://localhost:8080/jobs \
    -H "Content-Type: application/json" \
    -d '"'"'{"task_type":"segments","payload":{"segments":3}}'"'"' &&
  echo &&
  curl -sS -m 10 http://localhost:8080/jobs | head -c 200
'
```

Expected: JSON with `"id"` (not `job_id`), status `pending` → `completed`. Dashboard at `https://4orge.duckdns.org/dashboard` (HTTP 200).

---

## 5. Verify LLM Calls Work

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  J=$(curl -sS -m 10 -X POST http://localhost:8080/jobs \
    -H "Content-Type: application/json" \
    -d '"'"'{"task_type":"cp_solve","payload":{"prompt":"Find min subarray length with sum >= S. N=6 S=7 arr=2 3 1 2 4 3. Answer: 2.","language":"python"},"priority":9}'"'"' | python3 -c "import sys,json; print(json.load(sys.stdin)['"'"'id'"'"']") &&
  echo "job_id=$J" &&
  sleep 10 &&
  docker exec forge-postgres psql -U postgres -d forge -c "select step_number, step_type, status, worker_id from job_steps where job_id = '"'"'$J'"'"'::uuid order by step_number;"
'
```

Expected: 7+ steps (`plan`/`tool_call`), 5+ LLM calls in `llm_calls` table, backend=`groq`, no errors.

---

## 6. Run the Week 4 Crash-Recovery Demo

This proves `kill -9` on a worker mid-loop → different worker resumes from checkpoint.

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  cd ~/forge &&
  PROBLEM='"'"'Given an array of integers nums and an integer target, return indices of the two numbers such that they add up to target. You may assume exactly one solution. Write Python two_sum(nums, target). Constraints: 2 <= n <= 10000. Example: [2,7,11,15], target=9 -> [0,1]. Brute force O(n^2) times out. Search KB, write, test with run_tests, then finish.'"'"' \
  API_URL=https://4orge.duckdns.org \
  GRACE=0 \
  bash scripts/cp_solve_agent_demo.sh
'
```

### Expected PASS Output

```
== submitting a cp_solve agent job via https://4orge.duckdns.org ==
job_id=<uuid>
owner (first claim): worker-1
== waiting for the agent to commit its first plan (cap 60s) ==
  first plan committed (2 committed step(s))
== watching for the mid-iteration kill window (cap 60s) ==
  full iteration committed (2 steps); no mid-iteration window within 0s — killing mid-job
kill mode: post_iteration
== killing owner worker-1 (container forge-worker-1) with kill -9 ==
  killed; lease will expire and a different worker should reclaim
== waiting for reclaim + resume + completion (cap 300s) ==
  status=running claimed_by=worker-1 committed=2
  ...
  status=running claimed_by=worker-2 committed=2
  status=running claimed_by=worker-2 committed=6
  status=running claimed_by=worker-2 committed=8
  status=completed claimed_by=worker-2 committed=9
owner (final claim): worker-2
PASS: a different worker (worker-2) completed the job, not the killed worker-1
steps=9 types=plan,tool_call worker_ids=worker-1,worker-2 worker_boundaries=[2]
PASS: all 9 plan/tool_call steps committed exactly once, contiguous; two-worker trace; finish decision durable
== restarting the killed worker forge-worker-1 to restore the fleet ==
== PASS: kill -9 -> different worker resumed the agent from checkpoint, exactly-once, two-worker trace ==
```

Key assertions verified:
- Final `claimed_by` ≠ killed worker
- Step numbers 1..N contiguous, no duplicates
- Both `plan` and `tool_call` step types present
- ≥2 distinct `worker_id` in trace
- Final step is `plan` with `action: "finish"`

---

## 7. Common Gotchas

| Issue | Fix |
|-------|-----|
| `docker compose build worker` fails | Use `worker-1 worker-2 worker-3 worker-4` |
| `GROQ_API_KEY` unset | Add to `.env` (step 2) |
| `host.docker.internal` unresolvable | Use Groq (cloud); Ollama not installed on VM |
| Job completes before kill window | Use harder problem (`PROBLEM=...`) + `GRACE=0` |
| `job_id` vs `id` in API response | API returns `"id"`; demo script expects `"job_id"` locally but works via HTTPS |
| `docker exec -T` unsupported | Omit `-T` flag on Oracle VM's Docker |

---

## 8. Restore Fleet (after demo)

The demo script restarts the killed worker automatically (`STOP_KILLED` empty). Verify:

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org 'docker compose ps'
```

All 7 containers should be `Up`.

---

## One-Command Deploy (for reference)

```bash
# Run from your laptop — does steps 1-6 end-to-end
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  set -e
  cd ~/forge
  git pull origin main
  # (Assumes .env already has Groq key)
  docker compose build orchestrator worker-1 worker-2 worker-3 worker-4
  docker compose up -d
  curl -sS -X POST http://localhost:8080/jobs -H "Content-Type: application/json" -d '"'"'{"task_type":"segments","payload":{"segments":3}}'"'"'
  echo "Deploy OK"
'
```
---

## 9. Enable Dashboard Token Metrics (Rate-Limit Wrapper)

The live dashboard at \`https://4orge.duckdns.org/dashboard\` includes an **LLM Tokens** chart. Token metrics (\`forge_worker_llm_tokens_total\`) are recorded only when the \`RateLimitedBackend\` wrapper is active.

Enable the wrapper with deliberately generous budgets so the test records metrics without throttling normal agent activity:

\`\`\`bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  cd ~/forge &&
  sed -i '/^RATE_LIMIT_BACKEND=/d; /^RATE_LIMIT_TPM=/d; /^RATE_LIMIT_RPM=/d' .env &&
  cat >> .env <<'\''EOF'\''
RATE_LIMIT_BACKEND=memory
RATE_LIMIT_TPM=100000
RATE_LIMIT_RPM=100
EOF
  docker compose up -d --force-recreate worker-1 worker-2 worker-3 worker-4
'
\`\`\`

Confirm a worker has picked up both Groq and the wrapper:

\`\`\`bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  docker logs forge-worker-1 2>&1 | grep -E "LLM backend selected|rate limiting enabled"
'
\`\`\`

Expected log lines include \`backend=groq\` and \`rate limiting enabled tpm=100000 rpm=100\`.

---

## 10. Run the Groq LLM Dashboard Test

This test submits a real \`cp_solve\` agent job. It exercises the Groq backend, writes durable \`plan\` and \`tool_call\` steps, records LLM calls and token usage, and supplies live data for every dashboard chart.

### Submit the job

\`\`\`bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  J=$(curl -sS -m 10 -X POST http://localhost:8080/jobs \\
    -H "Content-Type: application/json" \\
    -d '\''{"task_type":"cp_solve","payload":{"prompt":"Given an array of positive integers nums and an integer target, find the minimal length of a contiguous subarray whose sum is >= target; return 0 if none. Example: nums=[2,3,1,2,4,3], target=7 -> 2. Write a Python function min_subarray_len(nums, target) using the sliding window technique. Constraints: 1 <= n <= 100000. Steps: search the KB for the sliding window pattern, write the solution, test it with run_tests using the example, then finish.","language":"python"},"priority":9}'\'' \
    | python3 -c "import json,sys; print(json.load(sys.stdin)[\"id\"])" ) &&
  echo "job_id=$J" &&
  sleep 15 &&
  curl -sS -m 10 http://localhost:8080/jobs/$J && echo &&
  curl -sS -m 10 http://localhost:8080/jobs/$J/llm_calls
'
\`\`\`

A completed job normally makes three or more calls. The exact count and duration vary because the agent may make different valid tool decisions.

### Verify the durable execution trace

\`\`\`bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  J=<job_id_from_previous_step>
  curl -sS -m 10 http://localhost:8080/jobs/$J/trace | python3 -m json.tool
'
\`\`\`

Expected: the job has status \`completed\`; every LLM call reports \`"backend":"groq"\`; and the trace contains at least one \`plan\` and one \`tool_call\` step.

### Verify metrics backing the dashboard

\`\`\`bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  echo "=== dashboard ==="
  curl -sS -o /dev/null -w "HTTP %{http_code}\\n" https://4orge.duckdns.org/dashboard
  echo "=== Groq token counters ==="
  docker exec forge-worker-1 wget -qO- http://localhost:9091/metrics \\
    | grep "forge_worker_llm_tokens_total.*backend=\\\"groq\\\"" || true
  echo "=== completed jobs ==="
  curl -sS http://localhost:8080/api/stats
'
\`\`\`

The specific worker that claims a job can vary. If the token counters are absent on \`forge-worker-1\`, run the same metrics command against \`forge-worker-2\`, \`forge-worker-3\`, and \`forge-worker-4\`.

### Dashboard acceptance criteria

Open \`https://4orge.duckdns.org/dashboard\` after the job completes and refresh once. Confirm:

- **Job Status:** the completed count increases and the status doughnut contains a completed segment.
- **Step Duration:** nonzero \`plan\` and/or \`tool_call\` histogram buckets are shown.
- **LLM Tokens:** prompt and completion bars appear under the \`groq\` backend.
- **Job Trace:** the submitted \`cp_solve\` job appears with its completed state and durable step history.
