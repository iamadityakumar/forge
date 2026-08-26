import json
import urllib.request
import time
import sys

BASE_URL = "http://localhost:8080" if len(sys.argv) < 2 else sys.argv[1]

jobs_to_publish = [
    # CP Solve Jobs
    {
        "task_type": "cp_solve",
        "payload": {
            "prompt": "Given a sorted array of integers nums and an integer target, write a function search_range(nums, target) that returns the starting and ending position of target using binary search in O(log n). Search KB for binary search, write code, run_tests, then finish.",
            "language": "python"
        },
        "priority": 9
    },
    {
        "task_type": "cp_solve",
        "payload": {
            "prompt": "Given n non-negative integers representing heights where each line is at coordinate (i, height[i]), find two lines that together with the x-axis form a container containing the most water. Write max_area(height) using two pointers in O(n). Search KB, write solution, run_tests, then finish.",
            "language": "python"
        },
        "priority": 8
    },
    {
        "task_type": "cp_solve",
        "payload": {
            "prompt": "Given an array of intervals [start, end], merge all overlapping intervals and return the non-overlapping intervals. Write merge_intervals(intervals) in Python. Search KB, write code, run_tests, then finish.",
            "language": "python"
        },
        "priority": 7
    },
    {
        "task_type": "cp_solve",
        "payload": {
            "prompt": "Given an integer array nums, return the length of the longest strictly increasing subsequence. Write length_of_lis(nums) in Python using dynamic programming or binary search patience sorting. Search KB, write code, run_tests, then finish.",
            "language": "python"
        },
        "priority": 6
    },
    # Segment computation jobs
    {
        "task_type": "segments",
        "payload": {
            "segments": 5,
            "complexity": "medium"
        },
        "priority": 5
    },
    {
        "task_type": "segments",
        "payload": {
            "segments": 8,
            "complexity": "high"
        },
        "priority": 4
    },
    {
        "task_type": "segments",
        "payload": {
            "segments": 3,
            "complexity": "low"
        },
        "priority": 3
    }
]

submitted = []
for i, job in enumerate(jobs_to_publish, 1):
    req = urllib.request.Request(
        f"{BASE_URL}/jobs",
        data=json.dumps(job).encode("utf-8"),
        headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req) as resp:
            res = json.loads(resp.read().decode("utf-8"))
            job_id = res.get("id") or res.get("job_id")
            print(f"[{i}/{len(jobs_to_publish)}] Published {job['task_type']} job {job_id} (priority={job['priority']})")
            submitted.append({
                "id": job_id,
                "type": job["task_type"],
                "priority": job["priority"]
            })
    except Exception as e:
        print(f"Error publishing job {i}: {e}")

print(f"\nPublished {len(submitted)} jobs.")
with open("/tmp/batch_submitted_jobs.json", "w") as f:
    json.dump(submitted, f)
