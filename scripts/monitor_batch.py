import json
import urllib.request
import time
import sys

BASE_URL = "http://localhost:8080"

with open("/tmp/batch_submitted_jobs.json", "r") as f:
    jobs = json.load(f)

print(f"Checking status for {len(jobs)} jobs in batch:")
for item in jobs:
    jid = item["id"]
    jtype = item["type"]
    prio = item["priority"]
    
    req = urllib.request.Request(f"{BASE_URL}/jobs/{jid}")
    with urllib.request.urlopen(req) as resp:
        job = json.loads(resp.read().decode())
    
    status = job.get("status")
    worker = job.get("claimed_by") or "none"
    attempts = job.get("attempt_count")
    
    extra = ""
    if jtype == "cp_solve":
        req_calls = urllib.request.Request(f"{BASE_URL}/jobs/{jid}/llm_calls")
        try:
            with urllib.request.urlopen(req_calls) as resp:
                calls = json.loads(resp.read().decode())
                extra = f" | LLM calls: {len(calls)}"
        except:
            pass
        
        req_trace = urllib.request.Request(f"{BASE_URL}/jobs/{jid}/trace")
        try:
            with urllib.request.urlopen(req_trace) as resp:
                trace = json.loads(resp.read().decode())
                steps = trace if isinstance(trace, list) else trace.get("steps", [])
                extra += f" | Steps: {len(steps)}"
        except:
            pass
            
    print(f" - [{jtype.upper()}] (prio {prio}) {jid}: status={status} worker={worker} (att={attempts}){extra}")
