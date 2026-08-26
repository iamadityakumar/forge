import urllib.request
import urllib.error
import json
import time

with open("/home/ubuntu/forge/.env") as f:
    for line in f:
        if line.startswith("GROQ_API_KEY="):
            key = line.strip().split("=", 1)[1]

system_prompt = """You are a competitive programming assistant running in an automated execution engine.
Your goal is to solve the competitive programming task provided in the problem description.

You MUST respond strictly with a valid JSON object. Do NOT wrap output in extra conversational text outside the JSON.

### Available Tools:
- **kb_search**: Search knowledge base
  Schema: {"query": "string"}
- **run_tests**: Run python test code
  Schema: {"code": "string", "language": "python"}

### Response Format:
To execute a tool, return JSON:
{
  "thought": "Your reasoning here...",
  "action": "tool",
  "tool_name": "<name_of_tool>",
  "tool_args": { ... arguments matching tool schema ... }
}

To finish and complete the task, return JSON:
{
  "thought": "Your reasoning here...",
  "action": "finish",
  "answer": "Your final detailed solution and explanation..."
}"""

user_problem = "Given an array of integers nums and an integer target, return indices of the two numbers such that they add up to target. Write Python two_sum(nums, target) using a hash map. Search the KB for hash map, write the solution, test it with run_tests, then finish."

models = ["openai/gpt-oss-20b", "qwen/qwen3.8-27b"]

for model in models:
    data = {
        "model": model,
        "messages": [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": user_problem}
        ],
        "response_format": {"type": "json_object"}
    }
    req = urllib.request.Request(
        "https://api.groq.com/openai/v1/chat/completions",
        data=json.dumps(data).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
            "User-Agent": "curl/7.81.0"
        }
    )
    try:
        t0 = time.time()
        with urllib.request.urlopen(req) as resp:
            dur = time.time() - t0
            res = json.loads(resp.read().decode("utf-8"))
            content = res['choices'][0]['message']['content']
            parsed = json.loads(content)
            print(f"=== Model {model} ({dur:.2f}s) ===")
            print("Action:", parsed.get("action"))
            print("Tool:", parsed.get("tool_name"))
            print("Thought:", parsed.get("thought"))
            print("Tool args:", parsed.get("tool_args"))
    except Exception as e:
        print(f"Error testing {model}:", e)
