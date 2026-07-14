-- PostgreSQL migration for the job system
-- Creates jobs, job_steps, and workers tables matching the full implementation plan

-- 1. jobs table
CREATE TABLE jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    priority INT NOT NULL DEFAULT 0,
    idempotency_key TEXT UNIQUE,
    claimed_by TEXT,
    lease_expires_at TIMESTAMPTZ,
    attempt_count INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 3,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2. job_steps table (each step related to a job)
CREATE TABLE job_steps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    step_number INT NOT NULL,
    step_type TEXT NOT NULL,  -- plan | tool_call | observation
    input JSONB,
    output JSONB,
    status TEXT NOT NULL,
    duration_ms INT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(job_id, step_number)
);

-- 3. workers table (metadata about background workers)
CREATE TABLE workers (
    id TEXT PRIMARY KEY,
    hostname TEXT,
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now(),
    status TEXT NOT NULL DEFAULT 'idle'
);

-- Indexes for performance
CREATE INDEX idx_job_steps_job_id ON job_steps(job_id);
CREATE INDEX idx_jobs_created_at ON jobs(created_at);