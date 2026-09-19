#!/usr/bin/env python3
"""Benchmark Ollama or Groq with and without supplied retrieval context."""
import argparse, json, os, statistics, time, urllib.request

def load_dotenv():
    try:
        with open('.env', encoding='utf-8') as file:
            for line in file:
                line = line.strip()
                if line and not line.startswith('#') and '=' in line:
                    key, value = line.split('=', 1)
                    os.environ.setdefault(key, value)
    except FileNotFoundError:
        pass

def request(provider, host, model, prompt, max_completion_tokens):
    if provider == 'groq':
        url = host.rstrip('/') + '/chat/completions'
        headers = {'Content-Type': 'application/json', 'Authorization': 'Bearer ' + os.environ['GROQ_API_KEY'], 'User-Agent': 'forge-rag-benchmark/1.0'}
        body = {'model': model, 'messages': [{'role': 'user', 'content': prompt}], 'temperature': 0, 'max_tokens': max_completion_tokens}
    else:
        url = host.rstrip('/') + '/api/chat'
        headers = {'Content-Type': 'application/json'}
        body = {'model': model, 'stream': False, 'messages': [{'role': 'user', 'content': prompt}]}
    req = urllib.request.Request(url, json.dumps(body).encode(), headers)
    started = time.perf_counter()
    with urllib.request.urlopen(req, timeout=180) as response:
        result = json.load(response)
    elapsed = time.perf_counter() - started
    if provider == 'groq':
        usage = result.get('usage', {})
        return {'prompt_eval_count': usage.get('prompt_tokens', 0), 'eval_count': usage.get('completion_tokens', 0)}, elapsed
    return result, elapsed

def main():
    load_dotenv()
    parser = argparse.ArgumentParser()
    parser.add_argument('--provider', choices=['ollama', 'groq'], default='ollama')
    parser.add_argument('--models', nargs='+', default=['llama3.1'])
    parser.add_argument('--host', default=os.getenv('OLLAMA_HOST', 'http://localhost:11434'))
    parser.add_argument('--prompt', default='Explain how to solve range sum queries.')
    parser.add_argument('--retrieval', default='')
    parser.add_argument('--runs', type=int, default=3)
    parser.add_argument('--max-completion-tokens', type=int, default=512)
    parser.add_argument('--delay-seconds', type=float, default=12)
    parser.add_argument('--output', default='rag-benchmark.json')
    args = parser.parse_args()
    if args.provider == 'groq':
        if not os.getenv('GROQ_API_KEY'):
            parser.error('GROQ_API_KEY is required for --provider groq')
        if args.host == os.getenv('OLLAMA_HOST', 'http://localhost:11434'):
            args.host = os.getenv('GROQ_BASE_URL', 'https://api.groq.com/openai/v1')
        if args.models == ['llama3.1']:
            args.models = [os.getenv('GROQ_MODEL', 'llama-3.1-8b-instant')]
    rows = []
    for model in args.models:
        for mode, context in (('without_retrieval', ''), ('with_retrieval', args.retrieval)):
            if mode == 'with_retrieval' and not context:
                continue
            for _ in range(args.runs):
                prompt = args.prompt if not context else args.prompt + '\n\nRetrieved context:\n' + context
                result, elapsed = request(args.provider, args.host, model, prompt, args.max_completion_tokens)
                completion = result.get('eval_count', 0)
                rows.append({'model': model, 'mode': mode, 'latency_seconds': elapsed, 'prompt_tokens': result.get('prompt_eval_count', 0), 'completion_tokens': completion, 'tokens_per_second': completion / elapsed if elapsed else 0, 'pass': None})
                if args.delay_seconds > 0 and not (model == args.models[-1] and mode == 'with_retrieval' and _ == args.runs - 1):
                    time.sleep(args.delay_seconds)
    summary = {}
    for key in sorted({(r['model'], r['mode']) for r in rows}):
        values = [r['latency_seconds'] for r in rows if (r['model'], r['mode']) == key]
        summary[f'{key[0]}:{key[1]}'] = {'p50_latency_seconds': statistics.median(values), 'p95_latency_seconds': sorted(values)[max(0, int(len(values) * .95) - 1)], 'runs': len(values)}
    with open(args.output, 'w', encoding='utf-8') as output:
        json.dump({'provider': args.provider, 'measurements': rows, 'summary': summary}, output, indent=2)
    print(json.dumps(summary, indent=2))

if __name__ == '__main__':
    main()
