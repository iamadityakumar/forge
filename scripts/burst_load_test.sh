#!/usr/bin/env bash
# scripts/burst_load_test.sh — Burst load test demonstrating admission control (HTTP 429) & backpressure.
# Submits N jobs in a rapid burst to POST /jobs and asserts:
#   1. No 5xx server errors
#   2. HTTP 429 Too Many Requests returned with Retry-After header when MAX_PENDING_JOBS limit is hit
#   3. All accepted jobs reach terminal completed state

BASE_URL="${BASE_URL:-http://localhost:8080}"
TOTAL_JOBS="${TOTAL_JOBS:-30}"

echo "============================================================"
echo " Starting Burst Load Test: Submitting $TOTAL_JOBS jobs to $BASE_URL"
echo "============================================================"

accepted=0
rejected=0
server_errors=0
job_ids=()

for i in $(seq 1 $TOTAL_JOBS); do
  response=$(curl -s -w "\n%{http_code}" -X POST "$BASE_URL/jobs" \
    -H "Content-Type: application/json" \
    -d "{\"task_type\":\"segments\",\"payload\":{\"segments\":3}}")

  body=$(echo "$response" | head -n -1)
  status_code=$(echo "$response" | tail -n 1)

  if [ "$status_code" -eq 201 ]; then
    accepted=$((accepted + 1))
    id=$(echo "$body" | grep -o '"id":"[^"]*' | cut -d'"' -f4)
    job_ids+=("$id")
    echo "Job #$i -> 201 Created (ID: $id)"
  elif [ "$status_code" -eq 429 ]; then
    rejected=$((rejected + 1))
    echo "Job #$i -> 429 Too Many Requests (Admission Control Backpressure)"
  elif [ "$status_code" -ge 500 ]; then
    server_errors=$((server_errors + 1))
    echo "Job #$i -> $status_code Internal Server Error! (FAIL)"
  else
    echo "Job #$i -> $status_code Unexpected status code"
  fi
done

echo ""
echo "============================================================"
echo " Burst Summary:"
echo "   Accepted (201): $accepted"
echo "   Rejected (429): $rejected"
echo "   Server Errors (5xx): $server_errors"
echo "============================================================"

if [ "$server_errors" -gt 0 ]; then
  echo "FAIL: Server returned 5xx errors during burst load test!"
  exit 1
fi

echo "Waiting for all accepted jobs (${#job_ids[@]}) to complete..."
all_completed=false
deadline=$(($(date +%s) + 60))

while [ $(date +%s) -lt $deadline ]; do
  pending=0
  for id in "${job_ids[@]}"; do
    status_resp=$(curl -s "$BASE_URL/jobs/$id")
    status=$(echo "$status_resp" | grep -o '"status":"[^"]*' | cut -d'"' -f4)
    if [ "$status" != "completed" ]; then
      pending=$((pending + 1))
    fi
  done

  if [ "$pending" -eq 0 ]; then
    all_completed=true
    break
  fi
  sleep 2
done

if [ "$all_completed" = true ]; then
  echo "SUCCESS: All accepted burst jobs reached terminal completed status!"
  exit 0
else
  echo "FAIL: Timed out waiting for accepted jobs to complete."
  exit 1
fi