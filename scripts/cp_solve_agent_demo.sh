#!/bin/bash
# cp_solve_agent_demo.sh — the Week 4 thesis demo: the agent loop under a kill -9.
#
# Proves the Week-4 story end to end:
#   1. Submit a cp_solve job — a real plan→tool-call→observe agent job. A worker claims it
#      and the agent starts committing plan / tool_call checkpoint rows to job_steps.
#   2. We `kill -9` (SIGKILL) that worker MID-LOOP — no graceful drain, so the job row is
#      left `running` with a non-expired lease and a partial plan/tool_call trace.
#   3. Once the lease expires, a DIFFERENT worker reclaims the row (lease_epoch bumps), calls
#      reconstructHistory to rebuild the conversation from the committed rows, and RESUMES
#      from the last checkpointed LLM decision. If the kill landed in the
#      [plan-committed, tool-committed) window, the reclaimer's FIRST committed step is a
#      tool_call — the LLM decision is recovered, not recomputed (zero LLM re-spend).
#   4. The job reaches `completed` and GET /jobs/{id}/trace shows every plan/tool_call step
#      committed EXACTLY ONCE (contiguous step numbers, no duplicates), attributed across
#      TWO distinct worker_id values — the cross-worker agent trace the Week-3 demo could
#      only narrate via job-level claimed_by.
#
# Usage (against the deployed Oracle VM over HTTPS):
#   bash scripts/cp_solve_agent_demo.sh
#
# Locally (docker compose stack):
#   API_URL=http://localhost:8080 bash scripts/cp_solve_agent_demo.sh
#
# Env:
#   API_URL            orchestrator base URL (default https://4orge.duckdns.org)
#   WORKER_GLOB        docker container-name glob matching worker containers
#                      (default "forge-worker-?", matches forge-worker-1..9)
#   PROBLEM            cp_solve problem text (default: a sliding-window problem that needs
#                      multiple tool iterations — see below)
#   FIRST_PLAN_TIMEOUT seconds to wait for the agent's first committed plan (default 60)
#   KILL_WAIT          max seconds to wait for a kill window after the first plan (default 60)
#   GRACE              after the first full iteration (>= 2 committed steps), keep waiting
#                      this many seconds for the mid-iteration window before killing anyway
#                      (default 10)
#   TIMEOUT            overall cap for reclaim+resume+complete (default 300)
#   DOCKER             command prefix for reaching/killing worker containers (default "docker")
#   STOP_KILLED        if non-empty, the killed worker is left stopped at the end (it is
#                      restarted by default to restore the fleet).
#
# Requires: curl, python3 (for JSON parsing), docker (to kill the worker container).

set -euo pipefail

API_URL="${API_URL:-https://4orge.duckdns.org}"
WORKER_GLOB="${WORKER_GLOB:-forge-worker-?}"
FIRST_PLAN_TIMEOUT="${FIRST_PLAN_TIMEOUT:-60}"
KILL_WAIT="${KILL_WAIT:-60}"
GRACE="${GRACE:-10}"
TIMEOUT="${TIMEOUT:-300}"
DOCKER="${DOCKER:-docker}"
STOP_KILLED="${STOP_KILLED:-}"

JQ_BIN="${JQ_BIN:-python3}"

# py extracts a field from the JSON on stdin: $1 is a python expression evaluated
# with `j` bound to the decoded object.
py() { "$JQ_BIN" -c "import sys,json; j=json.load(sys.stdin); print($1)"; }
err() { echo "FAIL: $*" >&2; exit 1; }

# Default problem: minimum-size subarray sum (sliding window). The constraints make a
# quadratic solution untenable, so the agent should search_kb (sliding_window.md is embedded
# in the KB) -> write a solution -> run_tests -> finish: a multi-iteration job we can kill.
PROBLEM_DEFAULT=$'Given a list of N positive integers and a target S, find the minimal length of a contiguous subarray whose sum is at least S. Return 0 if no such subarray exists.\n\nInput format (stdin):\n  line 1: two integers N and S\n  line 2: N space-separated integers\n\nOutput format (stdout): a single integer — the minimal length, or 0.\n\nConstraints:\n  1 <= N <= 100000\n  1 <= S <= 1000000000\n  1 <= each element <= 10000\n\nExample:\n  input:\n  6 7\n  2 3 1 2 4 3\n  output:\n  2\n\nExplanation: the subarray [4,3] sums to 7 and has length 2. Note the large N: a quadratic solution will time out. Search the knowledge base for the right pattern, then write and test your solution with the run_tests tool before finishing.'
PROBLEM="${PROBLEM:-$PROBLEM_DEFAULT}"

echo "== submitting a cp_solve agent job via $API_URL =="
PAYLOAD=$(python3 -c 'import json,sys; print(json.dumps({"task_type":"cp_solve","payload":{"prompt":sys.argv[1],"language":"python"},"priority":9}))' "$PROBLEM")
JOB_ID=$(curl -fsS -X POST "$API_URL/jobs" \
  -H 'Content-Type: application/json' \
  -d "$PAYLOAD" | py 'j["id"]')
[ -n "$JOB_ID" ] || err "no job id returned"
echo "job_id=$JOB_ID"

# Wait for a worker to claim & start it, then capture the owner (claimed_by).
deadline=$(( $(date +%s) + 15 ))
OWNER=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  OWNER=$(curl -fsS "$API_URL/jobs/$JOB_ID" | py 'j.get("claimed_by") or ""')
  [ -n "$OWNER" ] && break
  sleep 0.5
done
[ -n "$OWNER" ] || err "job was never claimed (are workers running?)"
echo "owner (first claim): $OWNER"

# Wait for the agent to commit its first plan (the first LLM decision). This is the floor
# below which killing proves nothing — with zero committed steps the reclaimer would just
# start from scratch and the trace would carry one worker_id.
echo "== waiting for the agent to commit its first plan (cap ${FIRST_PLAN_TIMEOUT}s) =="
deadline=$(( $(date +%s) + FIRST_PLAN_TIMEOUT ))
COUNT=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  COUNT=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace" | py 'len(j)')
  [ -n "$COUNT" ] || COUNT=0
  if [ "$COUNT" -ge 1 ]; then break; fi
  sleep 0.5
done
[ "$COUNT" -ge 1 ] || err "no agent plan committed within ${FIRST_PLAN_TIMEOUT}s — is a worker running and is the LLM backend reachable?"
echo "  first plan committed ($COUNT committed step(s))"

# --------------------------------------------------------------------------
# Kill-window hunting. The two-row protocol gives a strong preferred kill point: an ODD
# committed-step count (plan rows are odd, tool_call rows even) means the last row is a lone
# plan whose tool_call never committed — kill here and the reclaimer must REUSE that plan
# (its first committed step is a tool_call, zero LLM re-spend). We poll fast to catch it;
# if we only ever see even counts (a full iteration committed but the next plan not yet), we
# kill after GRACE seconds anyway once the job is demonstrably mid-loop (>= 2 steps). A job
# that completes before we kill is reported as a FAIL (the problem was solved too fast).
# --------------------------------------------------------------------------
echo "== watching for the mid-iteration kill window (cap ${KILL_WAIT}s) =="
deadline=$(( $(date +%s) + KILL_WAIT ))
WINDOW_START=-1
KILL_NOW=""
PREV=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  BODY=$(curl -fsS "$API_URL/jobs/$JOB_ID")
  STATUS=$(printf '%s' "$BODY" | py 'j["status"]')
  if [ "$STATUS" = "completed" ]; then
    err "job completed before the kill window — the model solved it too fast. Resubmit with a harder problem (PROBLEM=...)."
  fi
  if [ "$STATUS" = "failed" ]; then
    err "job failed before the kill window; check GET /jobs/$JOB_ID"
  fi
  CUR_OWNER=$(printf '%s' "$BODY" | py 'j.get("claimed_by") or ""')
  if [ -n "$CUR_OWNER" ] && [ "$CUR_OWNER" != "$OWNER" ]; then
    echo "  owner changed: $OWNER -> $CUR_OWNER (will kill the current holder)"
    OWNER="$CUR_OWNER"
  fi

  COUNT=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace" | py 'len(j)')
  [ -n "$COUNT" ] || COUNT=0

  # Money shot: odd count >= 3 = a trailing lone plan row (tool_call never committed).
  if [ "$COUNT" -ge 3 ] && [ $((COUNT % 2)) -eq 1 ]; then
    echo "  mid-iteration window caught: $COUNT committed steps, last row is a lone plan"
    KILL_NOW="mid_iteration"; break
  fi
  # Fallback: first full iteration committed; wait GRACE seconds for the window, then kill.
  if [ "$COUNT" -ge 2 ] && [ "$WINDOW_START" = "-1" ]; then
    WINDOW_START=$(date +%s)
  fi
  if [ "$COUNT" -ge 2 ] && [ "$WINDOW_START" -ne "-1" ]; then
    ELAPSED=$(( $(date +%s) - WINDOW_START ))
    if [ "$ELAPSED" -ge "$GRACE" ]; then
      echo "  full iteration committed ($COUNT steps); no mid-iteration window within ${GRACE}s — killing mid-job"
      KILL_NOW="post_iteration"; break
    fi
  fi

  if [ "$COUNT" -ne "$PREV" ]; then echo "  committed steps: $COUNT"; fi
  PREV=$COUNT
  sleep 0.3
done
if [ -z "$KILL_NOW" ]; then
  # Deadline hit without qualifying; kill at whatever we have (>= 1 plan committed).
  COUNT=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace" | py 'len(j)')
  if [ "$COUNT" -lt 1 ]; then err "no agent step committed within ${KILL_WAIT}s — is the LLM backend reachable?"; fi
  echo "  kill-window deadline reached at $COUNT committed steps — killing mid-job"
  KILL_NOW="deadline"
fi
echo "kill mode: $KILL_NOW"

# Map OWNER (e.g. "worker-3") -> its container name (e.g. "forge-worker-3"). The compose
# naming convention is forge-worker-N for WORKER_ID=worker-N. Try a direct name match first,
# then fall back to inspect-by-WORKER_ID-env among matching containers.
owner_num="${OWNER#worker-}"
CONTAINER=""
if [ -n "$owner_num" ]; then
  guess="forge-worker-$owner_num"
  if $DOCKER inspect "$guess" >/dev/null 2>&1; then
    CONTAINER="$guess"
  fi
fi
if [ -z "$CONTAINER" ]; then
  # Fall back: enumerate running containers, keep those whose name matches the glob, and
  # pick the one whose WORKER_ID env == OWNER. Robust to non-standard container naming.
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

# Disable the killed worker's auto-restart BEFORE the SIGKILL. With `restart: unless-stopped`,
# a plain `docker kill` would bring the worker back seconds later — and a fast-restarting
# worker could re-claim its OWN job (same WORKER_ID) after lease expiry, which would weaken
# the "a DIFFERENT worker resumed" thesis. `docker update --restart=no` stops the daemon from
# relaunching it; we SIGKILL the process so there is no graceful drain and the job row is left
# `running` with a live-looking lease — the hard crash the fencing story must survive.
echo "== killing owner $OWNER (container $CONTAINER) with kill -9 =="
$DOCKER update --restart=no "$CONTAINER" >/dev/null 2>&1 || \
  echo "  (warn: could not set --restart=no; killed worker may auto-respawn and re-claim)"
$DOCKER kill -s KILL "$CONTAINER" >/dev/null || err "could not kill $CONTAINER"
echo "  killed; lease will expire and a different worker should reclaim"

# Now wait: lease expires -> a DIFFERENT worker reclaims (epoch bumps, running->claimed) ->
# reconstructHistory rebuilds the conversation -> resumes the remaining iterations -> job
# completes. Poll status + claimed_by + committed-step count.
echo "== waiting for reclaim + resume + completion (cap ${TIMEOUT}s) =="
deadline=$(( $(date +%s) + TIMEOUT ))
DONE=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  BODY=$(curl -fsS "$API_URL/jobs/$JOB_ID")
  STATUS=$(printf '%s' "$BODY" | py 'j["status"]')
  NEWOWNER=$(printf '%s' "$BODY" | py 'j.get("claimed_by") or ""')
  STEPS=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace" | py 'len(j)')
  echo "  status=$STATUS claimed_by=$NEWOWNER committed=$STEPS"
  if [ "$STATUS" = "completed" ]; then DONE=1; break; fi
  if [ "$STATUS" = "failed" ]; then
    err "job went to 'failed' (likely dead-lettered); check /jobs/$JOB_ID"
  fi
  sleep 1
done
[ "$DONE" = "1" ] || err "job did not reach completed within ${TIMEOUT}s"

# --------------------------------------------------------------------------
# The headline assertions.
# --------------------------------------------------------------------------

# Headline 1: a DIFFERENT worker resumed the job. CompleteJob does NOT clear claimed_by, so
# the completing worker's ID is still on the row — assert it is not the one we killed.
FINAL_OWNER=$(curl -fsS "$API_URL/jobs/$JOB_ID" | py 'j.get("claimed_by") or ""')
echo "owner (final claim): $FINAL_OWNER"
if [ -n "$FINAL_OWNER" ] && [ "$FINAL_OWNER" != "$OWNER" ]; then
  echo "PASS: a different worker ($FINAL_OWNER) completed the job, not the killed $OWNER"
else
  err "the killed worker ($OWNER) is still the claimer ($FINAL_OWNER) — recovery did not hand the job to a different worker"
fi

# Headline 2: every plan/tool_call step committed exactly once, contiguous, completed; both
# step types present; per-step worker_id spans TWO distinct workers (the money shot); and the
# final plan row is a durable finish decision (the agent's last action, not a tool).
TRACE=$(curl -fsS "$API_URL/jobs/$JOB_ID/trace")
printf '%s' "$TRACE" | python3 -c '
import sys, json
steps = json.load(sys.stdin)
steps.sort(key=lambda s: s["step_number"])

# Contiguous, no duplicates.
nums = [s["step_number"] for s in steps]
assert nums == list(range(1, len(steps) + 1)), "step numbers not contiguous: %r" % (nums,)

# All completed.
unfinished = [s["step_number"] for s in steps if s.get("status") != "completed"]
assert not unfinished, "steps not completed: %r" % (unfinished,)

# Both protocol step types present.
types = set(s["step_type"] for s in steps)
assert "plan" in types and "tool_call" in types, "missing plan/tool_call in trace: %r" % sorted(types)

# Two distinct workers wrote the trace.
workers = sorted(set(s["worker_id"] for s in steps if s.get("worker_id")))
assert len(workers) >= 2, "expected >= 2 distinct worker_ids, got %r" % (workers,)

# The last plan row is a finish decision (tolerant of fenced/prose JSON like parseDecision).
def parse_decision(raw):
    if isinstance(raw, dict):
        return raw
    s = raw.strip()
    try:
        return json.loads(s)
    except Exception:
        pass
    i, j = s.find("{"), s.rfind("}")
    return json.loads(s[i:j + 1]) if i != -1 and j > i else None

last_plan = [s for s in steps if s["step_type"] == "plan"][-1]
d = parse_decision(last_plan.get("output"))
assert d and d.get("action") == "finish", "last plan is not a finish decision: %r" % (last_plan.get("output"),)

# Money shot (informational): where the worker changed, is the boundary plan -> tool_call?
# That transition means the reclaimer reused the checkpointed LLM decision instead of making
# a fresh LLM call. A tool_call -> plan boundary means the kill landed between iterations and
# the reclaimer made a fresh decision (still a valid crash recovery, just no zero-re-spend).
boundaries = [i for i in range(1, len(steps)) if steps[i].get("worker_id") != steps[i - 1].get("worker_id")]
money = [i for i in boundaries if steps[i - 1]["step_type"] == "plan" and steps[i]["step_type"] == "tool_call"]
print("steps=%d types=%s worker_ids=%s worker_boundaries=%s" % (len(steps), ",".join(sorted(types)), ",".join(workers), boundaries))
if money:
    print("MONEY SHOT: reclaimer'"'"'s first committed step is a tool_call — the checkpointed LLM decision was reused, not recomputed")
print("PASS: all %d plan/tool_call steps committed exactly once, contiguous; two-worker trace; finish decision durable" % len(steps))
'

# Restore the fleet: bring the killed worker back with its original restart policy (unless
# STOP_KILLED). It was left `restart=no` so a re-`compose up` would skip it; start it and
# restore unless-stopped so the demo leaves a healthy multi-worker stack.
if [ -z "$STOP_KILLED" ] && [ -n "$CONTAINER" ]; then
  echo "== restarting the killed worker $CONTAINER to restore the fleet =="
  $DOCKER update --restart=unless-stopped "$CONTAINER" >/dev/null 2>&1 || true
  $DOCKER start "$CONTAINER" >/dev/null 2>&1 || true
fi

echo "== PASS: kill -9 -> different worker resumed the agent from checkpoint, exactly-once, two-worker trace =="
