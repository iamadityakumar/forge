#!/bin/bash
set -euo pipefail
API="http://localhost:8080"

echo "=== Health ==="
curl -sS "$API/health"
echo

echo "=== Create Job 1 ==="
JOB1=$(curl -sS -X POST "$API/jobs" \
  -H 'Content-Type: application/json' \
  -d '{"task_type":"ping","payload":{"n":1}}')
echo "$JOB1"

echo "=== Create Job 2 ==="
JOB2=$(curl -sS -X POST "$API/jobs" \
  -H 'Content-Type: application/json' \
  -d '{"task_type":"summarize","payload":{"url":"https://example.com"},"priority":5}')
echo "$JOB2"

echo "=== Create Job 3 (idempotency test - same key) ==="
JOB3=$(curl -sS -X POST "$API/jobs" \
  -H 'Content-Type: application/json' \
  -d '{"task_type":"test-idem","payload":{},"idempotency_key":"idem-001"}')
echo "$JOB3"

echo "=== Create Job 3 duplicate (same idempotency key) ==="
JOB3DUP=$(curl -sS -X POST "$API/jobs" \
  -H 'Content-Type: application/json' \
  -d '{"task_type":"test-idem","payload":{},"idempotency_key":"idem-001"}')
echo "$JOB3DUP"

echo "=== Waiting 10s for worker to process ==="
sleep 10

echo "=== List all jobs ==="
curl -sS "$API/jobs" | python3 -m json.tool

echo "=== List completed jobs ==="
curl -sS "$API/jobs?status=completed" | python3 -m json.tool

echo "=== Done ==="
