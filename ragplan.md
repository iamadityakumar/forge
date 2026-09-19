# RAG and Groq benchmark plan

## Current status

### Phase 1 — Retrieval layer: implemented

- `docker-compose.yml` uses `pgvector/pgvector:pg15`.
- Migration `000008` creates `kb_chunks` with 768-dimensional embeddings and an HNSW cosine index.
- `llm.EmbeddingBackend` has Ollama and deterministic fake implementations.
- `cmd/ingest` chunks Markdown KB documents using 1,200-character chunks and 200-character overlap, then upserts vectors.
- PostgreSQL-backed `search_kb` performs top-5 vector retrieval; the embedded keyword fallback remains available for offline tests.
- Retrieval latency is exported through Prometheus.

Open follow-ups: record embedding calls in `llm_calls` with latency/token metadata, and add a dedicated retrieval trace span. Retrieval is currently visible in the tool-call step.

### Phase 2 — Evaluation: implemented, not yet measured against live vectors

- `eval/rag_dataset.json` contains 30 executable tasks and 40 labeled queries.
- `cmd/rag-eval` calculates recall@1/3/5, MRR, and task pass rate.
- It fails explicitly when PostgreSQL, pgvector, or the embedding service is unavailable.

No Phase 2 result artifact is included because the Ollama embedding service was unavailable during the environment run.

### Phase 3 — Groq benchmark: implemented and measured

- `scripts/rag_benchmark.py` supports Ollama and Groq, compares prompts with and without supplied retrieval context, and records latency, throughput, prompt tokens, and completion tokens.
- Completed run: `qwen/qwen3.8-27b`, Groq, two runs per condition, 256-token cap, 12-second spacing.
- Without retrieval: p50 latency `0.775s`; mean throughput `330.5 tokens/s`.
- With retrieval: p50 latency `0.761s`; mean throughput `336.8 tokens/s`.
- Total: 4 requests, 1,024 completion tokens, 154 prompt tokens.
- Raw artifact: `groq-qwen3.8-27b-rag-benchmark.json`.

These are latency/token measurements only. Pass rate is `null` because the generic benchmark prompt does not produce machine-executable code.

### Phase 4 — Documentation: implemented

- README contains setup, evaluation, benchmark commands, metric definitions, and the measured Groq result.

## Remaining work

1. Run `cmd/ingest` against live Ollama and `cmd/rag-eval` against the populated pgvector database; commit a result artifact only when produced by that live run.
2. Add the Phase 1 ledger and trace follow-ups.
3. Add a task-specific model evaluation protocol if pass rate is required.
4. Repeat the benchmark from a clean checkout for final reproducibility.

## Reproduction

```powershell
docker compose up -d postgres
go run ./cmd/ingest -dir internal/tools/kb
go run ./cmd/rag-eval --output eval-results.json

$env:GROQ_API_KEY = '<key supplied outside tracked files>'
python scripts/rag_benchmark.py --provider groq --models qwen/qwen3.8-27b `
  --retrieval 'Prefix sums answer static range sums in O(1) after O(N) preprocessing.' `
  --runs 2 --max-completion-tokens 256 --delay-seconds 12 `
  --output groq-qwen3.8-27b-rag-benchmark.json
```

Recall@k is the fraction of labeled queries whose expected source appears in the first k retrieved chunks. MRR is the mean reciprocal rank of the first relevant chunk. Task pass rate is the fraction of supplied programs whose tests pass.

The benchmark uses a completion cap and request spacing to stay within configured provider limits; it does not claim that this small sample is a statistically definitive model comparison.
