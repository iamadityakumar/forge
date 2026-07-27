-- Undo Week 3 fencing/checkpoint columns and restore the original claim index.

DROP INDEX IF EXISTS idx_jobs_expired_lease;
DROP INDEX IF EXISTS idx_jobs_pending;

ALTER TABLE jobs DROP COLUMN IF EXISTS dead_letter;
ALTER TABLE jobs DROP COLUMN IF EXISTS completed_at;
ALTER TABLE jobs DROP COLUMN IF EXISTS run_at;
ALTER TABLE jobs DROP COLUMN IF EXISTS lease_epoch;

-- Restore the original (non-partial) claim index from migration 000001.
CREATE INDEX IF NOT EXISTS idx_jobs_claimable ON jobs (status, priority DESC, created_at ASC);
