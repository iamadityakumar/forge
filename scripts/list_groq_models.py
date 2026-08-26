import urllib.request
import urllib.error
import json
import time

with open("/home/ubuntu/forge/.env") as f:
    for line in f:
        if line.startswith("GROQ_API_KEY="):
            key = line.strip().split("=", 1)[1]

test_models = [
    "openai/gpt-oss-20b",
    "openai/gpt-oss-120b",
    "qwen/qwen3.6-27b",
    "qwen/qwen3.8-27b",
    "groq/compound-mini"
]

for model in test_models:
    data = {
        "model": model,
        "messages": [
            {"role": "system", "content": "You are a competitive programming assistant. Output JSON with key 'result'."},
            {"role": "user", "content": "Return a JSON object with result: 'ok'."}
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
    t0 = time.time()
    try:
        with urllib.request.urlopen(req) as resp:
            dur = time.time() - t0
            res = json.loads(resp.read().decode("utf-8"))
            content = res['choices'][0]['message']['content']
            usage = res.get('usage', {})
            print(f"SUCCESS {model} in {dur:.2f}s | prompt_tokens={usage.get('prompt_tokens')} comp_tokens={usage.get('completion_tokens')} | output: {content.strip()}")
    except urllib.error.HTTPError as e:
        print(f"FAILED {model} (HTTP {e.code}): {e.read().decode('utf-8', errors='replace')}")
    except Exception as e:
        print(f"FAILED {model}: {e}")
