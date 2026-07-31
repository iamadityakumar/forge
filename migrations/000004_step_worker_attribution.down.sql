-- migrations/000004_step_worker_attribution.down.sql
ALTER TABLE job_steps DROP COLUMN IF EXISTS worker_id;
