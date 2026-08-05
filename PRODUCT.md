# Product

<!-- impeccable:product-schema 1 -->

## Platform

web — a headless backend service today: the only surface is the HTTP API (`/jobs`, `/jobs/{id}`, `/jobs/{id}/trace`, `/health`). A plain HTML/JS dashboard is planned for Week 6; it will be the first rendered UI and it is web.

## Users

- **The author (Aditya) — builder and primary user.** Situation: a backend/infra portfolio project built week by week to demonstrate distributed-systems craft to recruiters. Job: produce a real, link-shareable system where each mechanism (atomic claiming, fencing, crash recovery, cost-aware limiting) is defensible in an interview. *(assumed — the repo's "MNC Recruiter-Approved" framing and weekly demo checkpoints are strong evidence; the interview probe returned no confirmation.)*
- **Recruiters / interviewers — evaluators.** They interact through the live HTTPS deployment and demo scripts, not a product surface. *(assumed)*
- **Engineers who self-host AI-agent jobs — aspirational users.** The product is architected (interface abstractions, free-tier deploy, documented protocols) so it could be run by others; no such users exist yet. *(assumed)*

## Product Purpose

Forge is a self-hosted job orchestration system that runs multi-step AI agent tasks reliably across multiple workers — with checkpointed crash recovery, rate limiting, and live observability — deployed on entirely free infrastructure. Success today is a system that survives real failure (a worker `kill -9` mid-job) without losing or duplicating a single step, that spends against a real budget rather than a vague request count, and whose story and mechanisms carry an interview.

## Positioning

Reliable AI agent orchestration on $0/month infrastructure. The claim a neighboring job-queue could not truthfully copy: every LLM decision and tool execution is a durable, fenced, checkpointed row, so a killed worker's job resumes exactly-once from the last committed step — and the cost of that resilience is governed by a token-denominated budget enforced at the LLM call boundary, not at the request boundary.

## Operating Context

- **Week-plan-driven development.** The build is organized into weekly plans (`week1_plan.md` … `week5_plan.md`, `week6_plan.md` in a worktree), each with a thesis, phases, and a checkpoint/demo. `forge-implementation-plan.md` and `FORGE_COMPLETE_BUILD_PLAN.md` carry the overall trajectory to Week 9.
- **The canonical demo** (docs/week4_demo.md): submit a `cp_solve` job, watch the `plan → tool_call → observe` loop commit durable step rows, `kill -9` the owning worker mid-loop, and show a different worker reclaim the job (bumping `lease_epoch`), rebuild the conversation from `job_steps`, and resume with contiguous, exactly-once, two-worker step numbers.
- **Live deployment.** Oracle Cloud Always Free VMs; HTTPS at `4orge.duckdns.org` (DuckDNS + Caddy). SSH `ubuntu@4orge.duckdns.org -i ~/.ssh/forge_vm`; repo `~/forge` on `main`. See `deploy/ORACLE_VM.md`.
- **Local stack.** Docker Compose: `postgres`, `orchestrator`, `worker-1..4`. Migrations applied via `docker exec forge-postgres psql`. Worker images must contain `python3` for the `run_tests` tool. LLM backend is selectable: Ollama by default (containers default `OLLAMA_HOST` to `host.docker.internal:11434`), Groq via `LLM_BACKEND=groq` + `GROQ_API_KEY`.
- **CI** runs tests with the race detector on every push; the repo enforces ~80% test coverage and chaos/invariant tests.
- **Knowledge graph.** A `graphify` index (`graphify-out/`) is maintained after code changes; `graphify query/path/explain` are the first stop for codebase questions.

## Capabilities and Constraints

**Capabilities (shipped through Week 4, live on main):**

- Job API: `POST /jobs`, `GET /jobs/{id}`, `GET /jobs/{id}/trace`, `/health`, chi router.
- Durable Postgres queue: `SELECT … FOR UPDATE SKIP LOCKED` claiming, idempotency keys, priority, attempts/max_attempts.
- Atomic claiming + fencing: `lease_expires_at` self-renewing lease and `lease_epoch` fence token so a stale worker can never double-execute.
- Crash recovery: reclaim of expired `claimed`/`running` jobs; checkpointed `job_steps` (`plan`/`tool_call` rows, `worker_id` attribution, unique `(job_id, step_number)`) enable zero-loss, exactly-once resumption.
- LLM agent: `LLMBackend` interface with Ollama and Groq backends; `cp_solve` agent (competitive-programming solver) with `search_kb` and sandboxed `run_tests` tools; `plan`/`tool_call` step protocol.
- Resilience: transient-error retry with exponential backoff + jitter, dead-lettering for poison jobs, bounded per-worker concurrency (`WORKER_CONCURRENCY`), graceful release on `SIGTERM`.
- Deterministic testability: `FakeBackend` (scripted responses, `CallCount`), `ManualClock`, chaos invariant tests.

**In flight (Week 5, worktree `week5-ratelimit`, unmerged on `main`):**

- `internal/ratelimit/` token-bucket limiter wrapped as a `RateLimitedBackend` decorator; cost measured in estimated tokens against the provider's free-tier budget, then reconciled against real `usage` after each call.
- Admission control: `MAX_PENDING_JOBS` → `POST /jobs` returns `429` + `Retry-After` when the pending queue is full.
- Burst/load test script (`scripts/burst_load_test.sh`).

**Planned (Week 6):**

- Structured `slog` logging (`LOG_FORMAT`/`LOG_LEVEL`), Prometheus-format metrics per layer, enriched `/health`, and a plain-HTML/JS dashboard polling the real APIs. `web/` is greenfield. Stretch: OpenTelemetry distributed tracing.

**Constraints:**

- **$0/month hard constraint** — everything runs on genuinely free tiers (Neon Postgres, Oracle Cloud, Upstash Redis, Groq, Ollama, DuckDNS). No paid dependencies.
- LLM free-tier token budgets are the binding capacity, not requests-per-second — the reason cost-aware limiting exists.
- Minimal dependencies, stdlib-first (`net/http` + `chi`, `pgx`, `golang.org/x/sync`); every line intended to be defensible.
- No distributed (Redis-backed) limiter yet; the Week 5 limiter is single-node/in-memory. Upstash Redis is a planned stretch.
- Dev machine is Windows; production is Linux containers / Oracle ARM VMs. Go 1.25.
- No UI until Week 6. No README yet.

## Brand Commitments

- **Name:** Forge.
- **Pitch phrasing** ("self-hosted job orchestration … reliably … on entirely free infrastructure") is used consistently across plans and is worth preserving.
- **Process identity:** weekly thesis → phases → checkpoint demo is the project's rhythm; `STANDOUT_UPGRADES.md` (U1–U10) is the running ledger of the mechanisms that set it apart.
- No binding visual identity, palette, type, or voice exists (none established — the product has no UI).
- Never include Claude Code in commits or comments (project rule).

## Evidence on Hand

- **Live HTTPS demo + deployment:** `4orge.duckdns.org`; `deploy/ORACLE_VM.md`; SSH + re-run steps in project memory (`forge-oracle-vm-live.md`).
- **Week 4 crash-recovery demo (2026-07-31):** `docs/week4_demo.md`, `scripts/cp_solve_agent_demo.sh` — two-worker trace, exactly-once steps, zero LLM re-spend for committed `plan` rows.
- **Week 3 demo:** `docs/week3_demo.md`.
- **Chaos/invariant test:** `internal/worker/chaos_test.go` (kills workers mid-batch and asserts eventual, exactly-once completion).
- **API smoke test:** `scripts/verify_api.sh`.
- **Plans & narrative:** `forge-implementation-plan.md`, `FORGE_COMPLETE_BUILD_PLAN.md`, `STANDOUT_UPGRADES.md`, `week1_plan.md`–`week5_plan.md`.
- **Knowledge graph:** `graphify-out/` (GRAPH_REPORT.md, wiki).
- **Absences that must not be fabricated:** no README, no user testimonials, no real third-party customers, no pricing, no benchmark claims beyond the demo traces recorded above.

## Product Principles

1. **Backpressure over failure.** When capacity is dry (budget spent, queue full, lease held), the system waits or rejects early with a clear `429` + `Retry-After` — it never drops, corrupts, or double-processes a job.
2. **Durable correctness over in-memory convenience.** Every LLM decision and tool execution is a fenced, checkpointed row; recovery resumes from the last committed step, exactly once.
3. **Cost is a first-class capacity dimension.** Spend is denominated in tokens against the provider's real budget and enforced at the LLM call boundary — the difference between protecting an API and protecting a budget.
4. **Free infrastructure is a feature, not a hack.** $0/month on genuinely free tiers while still using production patterns (SKIP LOCKED, fencing, leases, checkpoints) real companies deploy.
5. **Every line defensible.** Minimal deps, stdlib-first, and each mechanism is a story someone can explain in an interview.

## Accessibility & Inclusion

No product-specific requirement established — the current surface is a headless API. Accessibility applies when the Week 6 dashboard ships; it must use plain, robust HTML with sensible contrast, keyboard operability, and reduced-motion consideration. *(assumed, not yet decided)*
