import json
import urllib.request
import time
import sys

BASE_URL = "http://localhost:8080"

with open("/tmp/submitted_jobs.json", "r") as f:
    job_ids = json.load(f)

for job_id in job_ids:
    req = urllib.request.Request(f"{BASE_URL}/jobs/{job_id}")
    with urllib.request.urlopen(req) as resp:
        job = json.loads(resp.read().decode())
    
    req_calls = urllib.request.Request(f"{BASE_URL}/jobs/{job_id}/llm_calls")
    with urllib.request.urlopen(req_calls) as resp:
        calls = json.loads(resp.read().decode())
    
    req_trace = urllib.request.Request(f"{BASE_URL}/jobs/{job_id}/trace")
    with urllib.request.urlopen(req_trace) as resp:
        trace = json.loads(resp.read().decode())
    
    print(f"\n==========================================")
    print(f"JOB ID: {job_id}")
    print(f"Status: {job.get('status')} | Worker: {job.get('claimed_by')} | Attempt: {job.get('attempt_count')}")
    if job.get('error_message'):
        print(f"Error: {job.get('error_message')}")
    
    print(f"LLM Calls ({len(calls)}):")
    for c in calls:
        print(f"  - [{c.get('backend')}] worker={c.get('worker_id')} latency={c.get('latency_ms')}ms prompt_tok={c.get('prompt_tokens')} comp_tok={c.get('completion_tokens')} err={c.get('error')}")
    
    steps = trace if isinstance(trace, list) else trace.get("steps", [])
    print(f"Committed Steps ({len(steps)}):")
    for s in steps:
        step_num = s.get('step_number')
        stype = s.get('step_type')
        w = s.get('worker_id')
        act = s.get('action') or ""
        tname = s.get('tool_name') or ""
        thought = s.get('thought') or ""
        print(f"  - Step {step_num} [{stype}] by {w}: action={act} tool={tname} thought={thought[:60]}...")
