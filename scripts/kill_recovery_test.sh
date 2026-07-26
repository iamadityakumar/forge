#!/bin/bash
# kill_recovery_test.sh — the Week 3 thesis demo (U1–U5 together).
#
# Proves the one story the whole project exists to make true:
#   1. Submit a long multi-segment job; a worker claims it and starts checkpointing
#      segments one at a time.
#   2. We `kill -9` (SIGKILL) that worker *mid-segment* — no graceful shutdown,
#      so the job row is left `running` with a non-expired lease and a partial
#      checkpoint.
#   3. Once that lease expires, a DIFFERENT worker reclaims the row: status
#      `running -> claimed`, `lease_epoch` bumps (a fresh fencing token), and it
#      reads `LastCompletedStep` and resumes from there — recovering after the
#      crash rather than restarting from scratch.
#   4. The job reaches `completed`, and `GET /jobs/{id}/trace` shows EVERY segment
#      checkpointed EXACTLY ONCE (no step executed twice) — Kleppmann fencing
#      tokens preventing double-execution by construction.
#
# This is the interview artifact: run it and watch the trace fill in after the kill.
#
# Usage (local single-host stack via docker compose):
#   WORKER_LEASE=10s bash scripts/kill_recovery_test.sh
#
#       (WORKER_LEASE=10s is read by docker-compose.yml so each worker's per-job
#        lease is 10s — reclaim happens ~10s after the kill instead of ~2m. The
#        workers themselves must be (re)started with this env; if a stack is
#        already up on the default 2m lease, `docker compose up -d` again to pick
#        it up, or set SEGMENTS very high and TIMEOUT generously.)
#
# Against the deployed Oracle VM over HTTPS:
#   API_URL=https://4orge.duckdns.org WORKER_GLOB=forge-worker-? \
#     WORKER_LEASE=10s bash scripts/kill_recovery_test.sh
#
# Env:
#   API_URL       orchestrator base URL (default http://localhost:8080)
#   WORKER_GLOB   docker compose service/container name glob to find the owning
#                 worker, e.g. "forge-worker-?" (matches forge-worker-1..9).
#   SEGMENTS      job length (default 20; each segment ~0.4–1.2s)
#   TIMEOUT       overall cap in seconds for reclaim+resume+complete (default 180)
#   KILL_WAIT     seconds to let the job checkpoint a few segments before the
#                 kill (default 3). The script also polls the trace for >=1
#                 completed step before killing, so this is a floor, not a hard
#                 guarantee of "mid-job".
#   DOCKER         command prefix for reaching/killing worker containers
#                  (default "docker"). Override e.g. DOCKER="docker compose -p
#                  forge exec -T" if your setup needs it.
#   STOP_KILLED    if non-empty, the killed worker is left stopped at the end
#                  (it's restarted by default to restore the fleet).
#
# Requires: curl, python3 (for JSON parsing), docker (to reach worker containers).

set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
WORKER_GLOB="${WORKER_GLOB:-forge-worker-?}"
SEGMENTS="${SEGMENTS:-20}"
TIMEOUT="${TIMEOUT:-180}"
KILL_WAIT="${KILL_WAIT:-3}"
DOCKER="${DOCKER:-docker}"
STOP_KILLED="${STOP_KILLED:-}"

JQ_BIN="${JQ_BIN:-python3}"

# py extracts a field from the JSON on stdin: $1 is a python expression evaluated
# with `j` bound to the decoded object.
py() { "$JQ_BIN" -c "import sys,json; j=json.load(sys.stdin); print($1)"; }
err() { echo "FAIL: $*" >&2; exit 1; }

echo "== submitting ${SEGMENTS}-segment job via ${API_URL} =="
JOB_ID=$(curl -fsS -X POST "$API_URL/jobs" \
  -H 'Content-Type: application/json' \
  -d "{\"task_type\":\"segments\",\"payload\":{\"segments\":${SEGMENTS}},\"priority\":9}" \
  | py 'j["id"]')
[ -n "$JOB_ID" ] || err "no job id returned"
echo "job_id=$JOB_ID"

# Wait for a worker to claim & start it, then capture the owner (claimed_by).
deadline=$(( $(date +%s) + 10 ))
OWNER=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  OWNER=$(curl -fsS "$API_URL/jobs/$JOB_ID" | py 'j.get("claimed_by") or ""')
  [ -n "$OWNER" ] && break
  sleep 0.5
done
[ -n "$OWNER" ] || err "job was never claimed (is a worker running?)"
echo "owner (first claim): $OWNER"

# Let it checkpoint a few segments before we kill the owner. Poll the trace so
# we know we're killing it *mid-job* (>=1 segment done, <SEGMENTS done) — killing
# before any segment ran would prove nothing about resume.
echo "== waiting for a partial checkpoint =="
deadline=$(( $(date +%s) + KILL_WAIT + 15 ))
CHECKPOINTED=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  CHECKPOINTED=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace" | py 'len(j)')
  echo "  checkpointed steps: $CHECKPOINTED / $SEGMENTS"
  if [ "$CHECKPOINTED" -ge 1 ] && [ "$CHECKPOINTED" -lt "$SEGMENTS" ]; then
    break
  fi
  sleep 0.5
done
[ "$CHECKPOINTED" -ge 1 ] || err "no segments checkpointed before kill — job may have completed instantly; raise SEGMENTS"
[ "$CHECKPOINTED" -lt "$SEGMENTS" ] || err "job already fully checkpointed before kill — raise SEGMENTS or lower WORKER_LEASE"
echo "  -> killing mid-job after $CHECKPOINTED checkpointed step(s)"

# Map OWNER (e.g. "worker-3") -> its container name (e.g. "forge-worker-3"). The
# compose naming convention is forge-worker-N for WORKER_ID=worker-N. Try a direct
# name match first, then fall back to inspect-by-WORKER_ID-env among matching
# containers.
owner_num="${OWNER#worker-}"
CONTAINER=""
if [ -n "$owner_num" ]; then
  guess="forge-worker-$owner_num"
  if $DOCKER inspect "$guess" >/dev/null 2>&1; then
    CONTAINER="$guess"
  fi
fi
if [ -z "$CONTAINER" ]; then
  # Fall back: enumerate running containers, keep those whose name matches the
  # glob pattern (case patterns support ? / * without filesystem globbing), and
  # pick the one whose WORKER_ID env == OWNER. This is robust to non-standard
  # container naming — the direct guess above only covers forge-worker-N.
  while read -r name; do
    [ -n "$name" ] || continue
    # shellcheck disable=SC2254 (case pattern matches the glob, not a filesystem glob)
    case "$name" in
      $WORKER_GLOB) : ;;
      *) continue ;;
    esac
    wid=$($DOCKER inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$name" 2>/dev/null \
      | sed -n 's/^WORKER_ID=//p' || true)
    if [ "$wid" = "$OWNER" ]; then
      CONTAINER="$name"; break
    fi
  done < <($DOCKER ps --format '{{.Names}}' 2>/dev/null)
fi
[ -n "$CONTAINER" ] || err "could not map owner '$OWNER' to a container (glob '$WORKER_GLOB'); set WORKER_GLOB or DOCKER"

# Disable the killed worker's auto-restart BEFORE the SIGKILL. With
# `restart: unless-stopped`, a plain `docker kill` would bring the worker back
# seconds later — and a fast-restarting worker could re-claim its OWN job (same
# WORKER_ID) after lease expiry, which would weaken the "a DIFFERENT worker
# resumed" thesis. `docker update --restart=no` stops the daemon from relaunching
# it; we SIGKILL the process so there is no graceful drain and the job row is
# left `running` with a live-looking lease — the hard crash the fencing story
# must survive.
echo "== killing owner $OWNER (container $CONTAINER) with kill -9 =="
$DOCKER update --restart=no "$CONTAINER" >/dev/null 2>&1 || \
  echo "  (warn: could not set --restart=no; killed worker may auto-respawn and re-claim)"
$DOCKER kill -s KILL "$CONTAINER" >/dev/null || err "could not kill $CONTAINER"
echo "  killed; lease will expire and a different worker should reclaim"

# Now wait: lease expires -> a DIFFERENT worker reclaims (epoch bumps,
# running->claimed) -> reads LastCompletedStep -> resumes the remaining segments
# -> job completes. Poll status + claimed_by + current step count.
echo "== waiting for reclaim + resume + completion (cap ${TIMEOUT}s) =="
deadline=$(( $(date +%s) + TIMEOUT ))
DONE=0
LAST_STEPS=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  BODY=$(curl -fsS "$API_URL/jobs/$JOB_ID")
  STATUS=$(printf '%s' "$BODY" | py 'j["status"]')
  NEWOWNER=$(printf '%s' "$BODY" | py 'j.get("claimed_by") or ""')
  STEPS=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace" | py 'len(j)')
  echo "  status=$STATUS claimed_by=$NEWOWNER checkpointed=$STEPS/$SEGMENTS"
  if [ "$STATUS" = "completed" ]; then DONE=1; break; fi
  if [ "$STATUS" = "failed" ]; then
    err "job went to 'failed' (likely dead-lettered); check /jobs/$JOB_ID"
  fi
  LAST_STEPS=$STEPS
  sleep 1
done
[ "$DONE" = "1" ] || err "job did not reach completed within ${TIMEOUT}s"

# --------------------------------------------------------------------------
# The three headline assertions.
# --------------------------------------------------------------------------

# Headline 1: a DIFFERENT worker resumed the job. CompleteJob does NOT clear
# claimed_by, so the completing worker's ID is still on the row — assert it is
# not the one we killed. (If CompleteJob ever starts clearing claimed_by, switch
# this assertion to read the final step's writer from a worker-id stamp on
# job_steps, or to "the owner changed at least once during recovery".)
FINAL_OWNER=$(curl -fsS "$API_URL/jobs/$JOB_ID" | py 'j.get("claimed_by") or ""')
echo "owner (final claim): $FINAL_OWNER"
if [ -n "$FINAL_OWNER" ] && [ "$FINAL_OWNER" != "$OWNER" ]; then
  echo "PASS: a different worker ($FINAL_OWNER) completed the job, not the killed $OWNER"
else
  err "the killed worker ($OWNER) is still the claimer ($FINAL_OWNER) — recovery did not hand the job to a different worker"
fi

# Headline 2: NO STEP EXECUTED TWICE — exactly-once-under-crash. Every
# checkpointed step must have a distinct step_number; a duplicate would mean a
# segment ran twice (a zombie re-awoke, or resume restarted instead of resumed).
TRACE=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace")
TOTAL=$(printf '%s' "$TRACE" | py 'len(j)')
UNIQUE=$(printf '%s' "$TRACE" | py 'len({s["step_number"] for s in j})')
echo "checkpointed steps: total=$TOTAL unique=$UNIQUE"
[ "$TOTAL" = "$UNIQUE" ] || err "a step was checkpointed more than once (not exactly-once)"

# Headline 3: every expected segment is present and completed, and exactly the
# set {1..SEGMENTS} — no gaps, no extras, all 'completed'.
printf '%s' "$TRACE" | "$JQ_BIN" -c '
import sys, json
steps = json.load(sys.stdin)
got = sorted(s["step_number"] for s in steps if s.get("status") == "completed")
expected = list(range(1, '"$SEGMENTS"' + 1))
assert got == expected, "step set mismatch: got {} expected {}".format(got, expected)
print("PASS: all " + str(len(got)) + " segments completed exactly once, no gaps")
'

# Restore the fleet: bring the killed worker back with its original restart
# policy (unless STOP_KILLED). It was left `restart=no` so a re-`compose up`
# would skip it; start it and restore unless-stopped so the demo leaves a
# healthy multi-worker stack.
if [ -z "$STOP_KILLED" ] && [ -n "$CONTAINER" ]; then
  echo "== restarting the killed worker $CONTAINER to restore the fleet =="
  $DOCKER update --restart=unless-stopped "$CONTAINER" >/dev/null 2>&1 || true
  $DOCKER start "$CONTAINER" >/dev/null 2>&1 || true
fi

echo "== PASS: kill -9 -> different worker resumed from checkpoint, exactly-once =="
