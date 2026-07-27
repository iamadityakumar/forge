-- Week 3: fencing tokens, scheduled retry (run_at), completion timestamp, dead-letter.
--
-- Replaces the non-partial idx_jobs_claimable with two partial indexes. A
-- single partial index cannot hold the whole reclaim predicate because
-- partial-index predicates must be IMMUTABLE and `now()` is STABLE — so the
-- expired-lease (`lease_expires_at < now()`) condition is split out into its
-- own index keyed on lease_expires_at.

ALTER TABLE jobs ADD COLUMN lease_epoch   INT         NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN run_at        TIMESTAMPTZ;
ALTER TABLE jobs ADD COLUMN completed_at  TIMESTAMPTZ;
ALTER TABLE jobs ADD COLUMN dead_letter   BOOLEAN     NOT NULL DEFAULT false;

DROP INDEX IF EXISTS idx_jobs_claimable;

-- Hot path: fresh + requeued 'pending' jobs. The run_at gate (run_at IS NULL OR
-- run_at <= now()) is applied as a post-scan filter by the planner; this index
-- only needs to narrow on status='pending'. Ordered to match the claim subselect's
-- ORDER BY priority DESC, created_at ASC.
CREATE INDEX idx_jobs_pending ON jobs (priority DESC, created_at ASC) WHERE status = 'pending';

-- Rare path: reclaim a job whose worker died holding a 'claimed' or 'running'
-- lease that has since expired. Small set; keyed on lease_expires_at.
CREATE INDEX idx_jobs_expired_lease ON jobs (lease_expires_at) WHERE status IN ('claimed', 'running');
