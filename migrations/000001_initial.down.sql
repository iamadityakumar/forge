-- Rollback migration 000001: drop all tables and indexes.

DROP INDEX IF EXISTS idx_jobs_claimable;
DROP INDEX IF EXISTS idx_jobs_created_at;
DROP INDEX IF EXISTS idx_job_steps_job_id;

DROP TABLE IF EXISTS job_steps;
DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS jobs;
