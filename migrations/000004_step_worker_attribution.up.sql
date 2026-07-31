-- migrations/000004_step_worker_attribution.up.sql
-- Week 4: per-step worker attribution. job_steps records which worker committed each
-- checkpoint so the agent trace can show a reclaimed job's plan/tool_call steps attributed
-- to TWO different workers — the cross-worker attribution the Week-3 demo narrated but
-- could only show via job-level claimed_by (the final owner).
ALTER TABLE job_steps ADD COLUMN worker_id TEXT;
