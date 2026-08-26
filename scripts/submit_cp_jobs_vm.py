import json
import urllib.request
import time
import sys

BASE_URL = "http://localhost:8080" if len(sys.argv) < 2 else sys.argv[1]

problems = [
    {
        "prompt": "Given an array of integers nums and an integer target, return indices of the two numbers such that they add up to target. Write Python two_sum(nums, target) using a hash map. Search the KB for hash map, write the solution, test it with run_tests, then finish.",
        "language": "python",
        "priority": 9
    },
    {
        "prompt": "Given an array of positive integers nums and an integer target, find the minimal length of a contiguous subarray whose sum is >= target; return 0 if none. Example: nums=[2,3,1,2,4,3], target=7 -> 2. Write Python min_subarray_len(nums, target) using sliding window. Search the KB for sliding window, write the solution, test with run_tests, then finish.",
        "language": "python",
        "priority": 8
    },
    {
        "prompt": "Given a string s containing brackets '()[]{}', determine if the input string is valid. Write Python is_valid(s) using a stack. Search the KB for stack matching, write the solution, test with run_tests, then finish.",
        "language": "python",
        "priority": 7
    }
]

submitted = []
for i, p in enumerate(problems, 1):
    payload = {
        "task_type": "cp_solve",
        "payload": {
            "prompt": p["prompt"],
            "language": p["language"]
        },
        "priority": p["priority"]
    }
    req = urllib.request.Request(
        f"{BASE_URL}/jobs",
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req) as resp:
            res = json.loads(resp.read().decode("utf-8"))
            job_id = res.get("id") or res.get("job_id")
            print(f"[{i}/{len(problems)}] Submitted job {job_id} (priority={p['priority']})")
            submitted.append(job_id)
    except Exception as e:
        print(f"Error submitting job {i}: {e}")

print(f"\nSuccessfully submitted {len(submitted)} jobs: {submitted}")
with open("/tmp/submitted_jobs.json", "w") as f:
    json.dump(submitted, f)
