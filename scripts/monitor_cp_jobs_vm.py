import json
import urllib.request
import time
import sys

BASE_URL = "http://localhost:8080" if len(sys.argv) < 2 else sys.argv[1]

try:
    with open("/tmp/submitted_jobs.json", "r") as f:
        job_ids = json.load(f)
except Exception as e:
    print("Could not load /tmp/submitted_jobs.json:", e)
    sys.exit(1)

print(f"Monitoring {len(job_ids)} jobs...")

def fetch(url):
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            return json.loads(r.read().decode("utf-8"))
    except Exception as e:
        return {"error": str(e)}

for job_id in job_ids:
    job = fetch(f"{BASE_URL}/jobs/{job_id}")
    trace = fetch(f"{BASE_URL}/jobs/{job_id}/trace")
    llm_calls = fetch(f"{BASE_URL}/jobs/{job_id}/llm_calls")
    
    status = job.get("status", "unknown")
    claimed_by = job.get("claimed_by", "none")
    steps = trace.get("steps", []) if isinstance(trace, dict) else []
    calls = llm_calls.get("calls", []) if isinstance(llm_calls, dict) else (llm_calls if isinstance(llm_calls, list) else [])
    
    print(f"\n--- Job {job_id} ---")
    print(f"Status: {status} | Claimed by: {claimed_by} | Steps count: {len(steps)} | LLM calls: {len(calls)}")
    for s in steps:
        step_num = s.get("step_number")
        step_type = s.get("step_type")
        worker = s.get("worker_id")
        action = s.get("action", "")
        print(f"  Step {step_num}: type={step_type} worker={worker} action={action}")
