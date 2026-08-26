# Forge Troubleshooting Runbook

Quick reference for diagnosing and fixing the most common Forge deployment failures, based on real incidents. Read the symptom, find the matching section, apply the fix, then verify.

---

## Symptom 1: Dashboard shows "System Degraded" and "0 Active Workers"

### Root Cause
The `/health` endpoint calls `CountActiveWorkers` which runs:

```sql
SELECT COUNT(*) FROM workers WHERE last_heartbeat > $2 - $1::interval
```

PostgreSQL infers the parameter type incorrectly and throws:

```
ERROR: operator does not exist: timestamp with time zone > interval (SQLSTATE 42883)
```

In the old code the error was swallowed (`workersOnline, _ :=`), so the API silently reported 0 workers and status `degraded`.

### Fix
1. Cast the timestamp parameter explicitly in `internal/store/postgres.go`:

```go
// Before (broken):
query := `SELECT COUNT(*) FROM workers WHERE last_heartbeat > $2 - $1::interval`

// After (fixed):
query := `SELECT COUNT(*) FROM workers WHERE last_heartbeat > $2::timestamptz - $1::interval`
```

2. Never swallow query errors in the health handler. In `internal/api/handlers.go`:

```go
// Before (silent failure):
workersOnline, _ := h.store.CountActiveWorkers(r.Context(), 30*time.Second)

// After (logs the real error):
workersOnline, err := h.store.CountActiveWorkers(r.Context(), 30*time.Second)
if err != nil {
    slog.Error("count active workers failed", "error", err)
    workersOnline = 0
}
```

3. Rebuild and restart the orchestrator:

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  cd ~/forge &&
  docker compose build orchestrator &&
  docker compose up -d orchestrator
'
```

### Verify
```bash
curl -sS http://localhost:8080/health
# Expected: {"db":"ok","status":"ok","workers_online":4}
```

Check the orchestrator logs show no `count active workers failed` errors:

```bash
docker logs forge-orchestrator --tail 50 | grep -i "count active workers"
```

---

## General Diagnostic Playbook

When any dashboard tile or API endpoint shows an unexpected value, follow this order:

### 1. Read the source of truth in the DB
Workers heartbeat every 10s into the `workers` table. If the dashboard says 0 but containers are up, verify the heartbeats are actually fresh:

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  docker exec forge-postgres psql -U postgres -d forge \
    -c "SELECT id, hostname, last_heartbeat, status FROM workers ORDER BY last_heartbeat DESC;"
'
```

If `last_heartbeat` is stale (older than ~30s), the worker heartbeat loop is broken — check worker logs:

```bash
docker logs forge-worker-1 --tail 50
```

### 2. Reproduce the failing query manually
The Go code may build SQL that looks fine but fails at runtime due to parameter type inference. Run the exact query pattern in psql:

```bash
docker exec forge-postgres psql -U postgres -d forge \
  -c "SELECT COUNT(*) FROM workers WHERE last_heartbeat > NOW() - INTERVAL '30 seconds';"
```

### 3. Never trust silently ignored errors
Anywhere the code does:

```go
val, _ := s.SomeQuery(ctx)
```

Replace it with explicit error handling and `slog.Error(...)`. A silent `_` turns a database bug into a wrong-looking dashboard.

### 4. Check the API error log
```bash
docker logs forge-orchestrator --tail 100
```

Look for lines like `level=ERROR` — they now contain the real Postgres error when the handler logs it.

### 5. Verify the metric pipeline end-to-end
```bash
# Orchestrator gauge (should be 4 with 4 healthy workers)
curl -sS http://localhost:8080/metrics | grep active_workers

# Worker gauges
curl -sS http://localhost:9091/metrics | grep active_workers
```

---

## Symptom 2: `docker compose build` output shows "no such service"

### Root Cause
The compose file uses explicit worker services `worker-1` ... `worker-4`, not a single `worker` service.

### Fix
```bash
docker compose build orchestrator worker-1 worker-2 worker-3 worker-4
docker compose up -d --force-recreate worker-1 worker-2 worker-3 worker-4
```

---

## Symptom 3: Worker claims jobs but job never completes

### Check
1. Is the worker still alive? `docker compose ps` — all should be `Up`.
2. Does the lease expire mid-job? Check for `lease renewal fenced` in worker logs.
3. Did the worker fail to start the LLM backend? Check startup logs:

```bash
docker logs forge-worker-1 2>&1 | grep -E "LLM backend selected|rate limiting enabled|worker started"
```

---

## Symptom 4: LLM token chart is empty

### Root Cause
Token metrics are only recorded when the `RateLimitedBackend` wrapper is active (i.e. `RATE_LIMIT_BACKEND != off` and budgets are set).

### Fix
```bash
cd ~/forge &&
sed -i '/^RATE_LIMIT_BACKEND=/d; /^RATE_LIMIT_TPM=/d; /^RATE_LIMIT_RPM=/d' .env &&
cat >> .env <<'EOF'
RATE_LIMIT_BACKEND=memory
RATE_LIMIT_TPM=100000
RATE_LIMIT_RPM=100
EOF
docker compose up -d --force-recreate worker-1 worker-2 worker-3 worker-4
```

---

---

## Symptom 5: Dashboard rate-limit waits jump up and down by 1

### Root Cause
The dashboard was making two requests per worker every refresh: one for the worker card and one for the aggregated metrics. A transient proxy failure cleared a worker's cached Prometheus counters, so the summed `rate_limit_waits_total` value temporarily dropped and then recovered. Overlapping refreshes could also merge the same worker sample more than once.

### Fix
The dashboard now uses one shared worker-metrics fetch per refresh, prevents overlapping refreshes, and retains the last-known-good worker counter sample during a transient fetch failure. Rebuild/redeploy the dashboard with the orchestrator image so the updated `web/dashboard.js` is served.

### Verify
Open the browser developer console and confirm each 5-second refresh makes one request per worker to `/api/worker-metrics/{worker}`. The `Rate limit waits` value should be monotonic for the lifetime of each worker process; it resets only when that worker restarts.

If the value still changes downward, check for worker restarts and proxy errors:

```bash
docker compose ps
docker logs forge-orchestrator --tail 100 | grep -i "worker\|metrics\|offline"
```

---

## Symptom 6: `invalid JSON body` when POSTing to /jobs

### Cause
Shell quoting mangles the JSON (especially `>=`, `->`, `<=` in prompts). PowerShell is the usual culprit.

### Fix
Write the JSON to a file first, then post it:

```bash
cat > /tmp/job.json <<'EOF'
{"task_type":"cp_solve","payload":{"prompt":"...","language":"python"},"priority":9}
EOF
curl -sS -X POST http://localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d @/tmp/job.json
```

---

## One-Command Health Check

```bash
ssh -i ~/.ssh/forge_vm ubuntu@4orge.duckdns.org '
  echo "=== health ===" && curl -sS http://localhost:8080/health
  echo && echo "=== stats ===" && curl -sS http://localhost:8080/api/stats
  echo && echo "=== workers ===" && curl -sS http://localhost:8080/api/workers
  echo && echo "=== containers ===" && docker compose ps
'
```

Expected: `status: ok`, `workers_online: 4`, all 7 containers `Up`.

---

## Fix Checklist (what was changed in the 2026-08-12 incident)

| File | Change |
|------|--------|
| `internal/store/postgres.go` | Cast `$2::timestamptz` in `CountActiveWorkers` query |
| `internal/api/handlers.go` | Log errors from `CountActiveWorkers` instead of swallowing them |

Always commit these two fixes together — the SQL fix makes the query work, the logging fix makes the *next* SQL bug visible immediately.
