#!/bin/bash
# drain_test.sh — Submit 20 jobs, wait for worker to drain, verify all completed.
# Usage: ./scripts/drain_test.sh [API_URL]
# Example: ./scripts/drain_test.sh http://localhost:8080
#          ./scripts/drain_test.sh https://4orge.duckdns.org
set -euo pipefail

API="${1:-http://localhost:8080}"
COUNT=20

echo "==> Submitting $COUNT jobs to $API ..."
for i in $(seq 1 "$COUNT"); do
  curl -sS -X POST "$API/jobs" \
    -H 'Content-Type: application/json' \
    -d "{\"task_type\":\"drain-test-$i\",\"payload\":{\"n\":$i}}" \
    -o /dev/null &
done
wait
echo "==> All $COUNT jobs submitted."

echo "==> Waiting 45s for worker to drain ..."
sleep 45

echo "==> Checking results ..."
TOTAL=$(curl -sS "$API/jobs" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
COMPLETED=$(curl -sS "$API/jobs?status=completed" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
FAILED=$(curl -sS "$API/jobs?status=failed" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")
PENDING=$(curl -sS "$API/jobs?status=pending" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))")

echo "Total:     $TOTAL"
echo "Completed: $COMPLETED"
echo "Failed:    $FAILED"
echo "Pending:   $PENDING"

if [ "$COMPLETED" -ge "$COUNT" ]; then
  echo "✅ PASS: All $COUNT jobs completed."
  exit 0
else
  echo "❌ FAIL: Only $COMPLETED / $COUNT completed."
  exit 1
fi
