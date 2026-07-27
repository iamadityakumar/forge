package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver registered as "pgx"
)

// PgStore implements JobStore backed by PostgreSQL.
type PgStore struct {
	db *sql.DB
}

// NewPgStore opens a connection pool and verifies connectivity.
func NewPgStore(databaseURL string) (*PgStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &PgStore{db: db}, nil
}

// DB returns the underlying *sql.DB for health checks or direct access.
func (s *PgStore) DB() *sql.DB { return s.db }

// Close closes the connection pool.
func (s *PgStore) Close() error { return s.db.Close() }

// Ping checks database connectivity.
func (s *PgStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// ---------------------------------------------------------------------------
// CreateJob
// ---------------------------------------------------------------------------

func (s *PgStore) CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (Job, error) {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	var idemKey *string
	if idempotencyKey != "" {
		idemKey = &idempotencyKey
	}

	// ON CONFLICT: if the idempotency key already exists, return the existing
	// row unchanged. The DO UPDATE SET id = jobs.id trick forces RETURNING to
	// fire even on conflict.
	query := `
		INSERT INTO jobs (task_type, payload, priority, idempotency_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO UPDATE SET id = jobs.id
		RETURNING id, task_type, payload, status, priority, idempotency_key,
		          claimed_by, lease_expires_at, attempt_count, max_attempts,
		          error_message, created_at, lease_epoch, run_at, completed_at, dead_letter
	`

	var job Job
	err := s.db.QueryRowContext(ctx, query, taskType, payload, priority, idemKey).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
		&job.LeaseEpoch, &job.RunAt, &job.CompletedAt, &job.DeadLetter,
	)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// GetJob
// ---------------------------------------------------------------------------

func (s *PgStore) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	query := `
		SELECT id, task_type, payload, status, priority, idempotency_key,
		       claimed_by, lease_expires_at, attempt_count, max_attempts,
		       error_message, created_at, lease_epoch, run_at, completed_at, dead_letter
		FROM jobs WHERE id = $1
	`
	var job Job
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
		&job.LeaseEpoch, &job.RunAt, &job.CompletedAt, &job.DeadLetter,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// ListJobs
// ---------------------------------------------------------------------------

// ListJobs returns jobs filtered by status. Empty status = all jobs.
// status == StatusDeadLetter ("dead_letter") is a VIRTUAL filter: it returns
// jobs that exhausted max_attempts (dead_letter=true), regardless of their
// real status('failed'). This is how GET /jobs?status=dead_letter surfaces the
// dead-letter queue (U5).
func (s *PgStore) ListJobs(ctx context.Context, status string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}

	var query string
	switch status {
	case StatusDeadLetter:
		// Virtual dead-letter filter: poisoned jobs at any real status, though
		// FailJob stores them at status='failed'.
		query = `
			SELECT id, task_type, payload, status, priority, idempotency_key,
			       claimed_by, lease_expires_at, attempt_count, max_attempts,
			       error_message, created_at, lease_epoch, run_at, completed_at, dead_letter
			FROM jobs
			WHERE dead_letter = true
			ORDER BY created_at DESC
			LIMIT $1
		`
		return s.scanJobs(ctx, query, limit)
	case "":
		query = `
			SELECT id, task_type, payload, status, priority, idempotency_key,
			       claimed_by, lease_expires_at, attempt_count, max_attempts,
			       error_message, created_at, lease_epoch, run_at, completed_at, dead_letter
			FROM jobs
			ORDER BY created_at DESC
			LIMIT $1
		`
		return s.scanJobs(ctx, query, limit)
	default:
		query = `
			SELECT id, task_type, payload, status, priority, idempotency_key,
			       claimed_by, lease_expires_at, attempt_count, max_attempts,
			       error_message, created_at, lease_epoch, run_at, completed_at, dead_letter
			FROM jobs
			WHERE status = $1
			ORDER BY created_at DESC
			LIMIT $2
		`
		return s.scanJobs(ctx, query, status, limit)
	}
}

// scanJobs runs a job SELECT with the given args and scans the result.
func (s *PgStore) scanJobs(ctx context.Context, query string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(
			&j.ID, &j.TaskType, &j.Payload, &j.Status,
			&j.Priority, &j.IdempotencyKey, &j.ClaimedBy,
			&j.LeaseExpiresAt, &j.AttemptCount, &j.MaxAttempts,
			&j.ErrorMessage, &j.CreatedAt,
			&j.LeaseEpoch, &j.RunAt, &j.CompletedAt, &j.DeadLetter,
		); err != nil {
			return nil, fmt.Errorf("scan job row: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}
	return jobs, nil
}

// ---------------------------------------------------------------------------
// ClaimJob — the core engineering artifact (SKIP LOCKED + fencing token)
// ---------------------------------------------------------------------------
//
// Mints a fencing token (lease_epoch = lease_epoch + 1) returned to the caller,
// who must present it on every subsequent fenced write. Reclaims both 'claimed'
// and 'running' jobs whose lease has expired (U3: a worker that crashes after
// StartJob leaves the row in 'running'; the textbook query that only reclaims
// 'claimed' loses it forever). Gates scheduled/retried jobs by run_at (U5): a
// job with run_at in the future is not claimable until it elapses. FOR UPDATE
// SKIP LOCKED ensures two workers never claim the same row.

func (s *PgStore) ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*Job, error) {
	query := `
		UPDATE jobs
		SET status           = 'claimed',
		    claimed_by       = $1,
		    lease_expires_at = now() + $2::interval,
		    lease_epoch      = lease_epoch + 1,
		    attempt_count    = attempt_count + 1,
		    run_at           = NULL
		WHERE id = (
			SELECT id FROM jobs
			WHERE (status = 'pending'
			       OR (status IN ('claimed','running') AND lease_expires_at < now()))
			  AND (run_at IS NULL OR run_at <= now())
			ORDER BY priority DESC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, task_type, payload, status, priority, idempotency_key,
		          claimed_by, lease_expires_at, attempt_count, max_attempts,
		          error_message, created_at, lease_epoch, run_at, completed_at, dead_letter
	`

	// Format lease duration as a Postgres interval in ms (precise for short
	// leases used in tests/demos; "60000 milliseconds" == 1 minute).
	interval := fmt.Sprintf("%d milliseconds", leaseDuration.Milliseconds())

	var job Job
	err := s.db.QueryRowContext(ctx, query, workerID, interval).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
		&job.LeaseEpoch, &job.RunAt, &job.CompletedAt, &job.DeadLetter,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // no claimable job — not an error
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &job, nil
}

// ---------------------------------------------------------------------------
// State transitions (fenced by lease_epoch — U1)
// ---------------------------------------------------------------------------

// StartJob transitions a job from claimed → running. The epoch must match the
// caller's fencing token; a mismatch (caller was deposed) yields ErrFenced.
func (s *PgStore) StartJob(ctx context.Context, jobID uuid.UUID, epoch int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'running'
		 WHERE id = $1 AND status = 'claimed' AND lease_epoch = $2`,
		jobID, epoch,
	)
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyZeroRows(ctx, jobID, epoch)
	}
	return nil
}

// CompleteJob transitions a job from running → completed, fenced by epoch.
func (s *PgStore) CompleteJob(ctx context.Context, jobID uuid.UUID, epoch int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'completed', completed_at = now()
		 WHERE id = $1 AND status = 'running' AND lease_epoch = $2`,
		jobID, epoch,
	)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyZeroRows(ctx, jobID, epoch)
	}
	return nil
}

// Retry backoff policy for requeued jobs. Package-level so tests can tighten
// it for deterministic/tight timing (a future Phase-7 Clock/FakeLlm seam will
// make this fully injection-based; for now overridable vars suffice).
//
//	backoff = min(cap, base * 2^(attempts-1)) + jitter
//
// 'attempts' is attempt_count at Fail time (the attempt that just failed,
// already incremented by ClaimJob). A poisoned job therefore retries with
// growing delay; once attempt_count >= max_attempts FailJob dead-letters
// instead of requeuing.
var (
	backoffBase   = 2 * time.Second
	backoffCap    = 5 * time.Minute
	backoffJitter func() time.Duration = defaultBackoffJitter
)

// defaultBackoffJitter adds up to backoffBase of uniform jitter to a retry to
// prevent thundering-herd retry storms. math/rand's top-level source is
// concurrency-safe (locked) since Go 1.20, so this is fine under -race.
func defaultBackoffJitter() time.Duration {
	return time.Duration(rand.Int63n(int64(backoffBase) + 1)) // [0, base]
}

func computeBackoff(attempts int) time.Duration {
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 20 { // guard against 1<<k overflow for absurd attempt counts
		shift = 20
	}
	d := backoffBase * (1 << shift)
	if d > backoffCap {
		d = backoffCap
	}
	if j := backoffJitter(); j > 0 {
		if d+j > backoffCap {
			d = backoffCap
		} else {
			d += j
		}
	}
	return d
}

// FailJob records a job's failure, fenced by epoch, and either requeues it for a
// scheduled retry or dead-letters it:
//
//   - attempt_count < max_attempts → requeue: status='pending', claimed_by and
//     lease cleared, run_at = now()+backoff, lease_epoch bumped (invalidating
//     the old token). The run_at gate in ClaimJob (run_at <= now()) enforces the
//     backoff delay before the job is claimable again.
//   - attempt_count >= max_attempts → dead-letter: status='failed',
//     dead_letter=true, completed_at=now(), recorded reason. Surfaced by
//     GET /jobs?status=dead_letter.
//
// A deposed worker (epoch mismatch, or reclaimed between the read and the write)
// gets ErrFenced and must not mutate the job. Reads attempt_count in a first
// statement so the backoff can be computed in Go (keeping testability); the
// fenced UPDATE then re-validates the epoch, so a race between read and write is
// safe (the UPDATE affects zero rows → ErrFenced, and the computed backoff is
// simply discarded).
func (s *PgStore) FailJob(ctx context.Context, jobID uuid.UUID, epoch int, reason string) error {
	var attemptCount, maxAttempts int
	err := s.db.QueryRowContext(ctx,
		`SELECT attempt_count, max_attempts FROM jobs
		 WHERE id = $1 AND lease_epoch = $2 AND status = 'running'`,
		jobID, epoch,
	).Scan(&attemptCount, &maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return s.classifyZeroRows(ctx, jobID, epoch)
	}
	if err != nil {
		return fmt.Errorf("fail job (read): %w", err)
	}

	if attemptCount >= maxAttempts {
		// Terminal: dead-letter the poison message.
		res, err := s.db.ExecContext(ctx,
			`UPDATE jobs
			    SET status = 'failed', dead_letter = true, completed_at = now(),
			        error_message = $3, claimed_by = NULL, lease_expires_at = NULL,
			        run_at = NULL
			  WHERE id = $1 AND lease_epoch = $2 AND status = 'running'`,
			jobID, epoch, reason,
		)
		if err != nil {
			return fmt.Errorf("fail job (dead-letter): %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return s.classifyZeroRows(ctx, jobID, epoch)
		}
		return nil
	}

	// Requeue: schedule a retry after backoff. Bumping lease_epoch invalidates
	// the just-failed worker's fencing token.
	delay := computeBackoff(attemptCount)
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs
		    SET status = 'pending', claimed_by = NULL, lease_expires_at = NULL,
		        run_at = now() + $3::interval, error_message = $4,
		        lease_epoch = lease_epoch + 1
		  WHERE id = $1 AND lease_epoch = $2 AND status = 'running'`,
		jobID, epoch, fmt.Sprintf("%d milliseconds", delay.Milliseconds()), reason,
	)
	if err != nil {
		return fmt.Errorf("fail job (requeue): %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyZeroRows(ctx, jobID, epoch)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Checkpointed steps (U4)
// ---------------------------------------------------------------------------

// RecordStep checkpoint-writes a single step, fenced by epoch. The C TE makes
// the insert conditional on the caller still owning the job (lease_epoch
// match); 0 rows ⇒ the caller was fenced ⇒ ErrFenced. ON CONFLICT makes a
// re-recording of the same step_number an in-place update (idempotent), so a
// zombie that re-awakes cannot create a duplicate or corrupt rows.
func (s *PgStore) RecordStep(ctx context.Context, jobID uuid.UUID, epoch int, step JobStep) (uuid.UUID, error) {
	query := `
		WITH owned AS (
		    SELECT 1 FROM jobs WHERE id = $1 AND lease_epoch = $2 FOR UPDATE
		)
		INSERT INTO job_steps (job_id, step_number, step_type, input, output, status, duration_ms)
		SELECT $1, $3, $4, $5, $6, 'completed', $7 FROM owned
		ON CONFLICT (job_id, step_number) DO UPDATE
		SET output = EXCLUDED.output, status = EXCLUDED.status, duration_ms = EXCLUDED.duration_ms
		RETURNING id
	`
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx, query,
		jobID, epoch, step.StepNumber, step.StepType, step.Input, step.Output, step.DurationMs,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrFenced
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("record step: %w", err)
	}
	return id, nil
}

// LastCompletedStep returns the highest step_number recorded as 'completed' for
// the job, or 0 if none. A reclaimed job resumes from this + 1.
func (s *PgStore) LastCompletedStep(ctx context.Context, jobID uuid.UUID) (int, error) {
	var step int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step_number), 0) FROM job_steps
		 WHERE job_id = $1 AND status = 'completed'`,
		jobID,
	).Scan(&step)
	if err != nil {
		return 0, fmt.Errorf("last completed step: %w", err)
	}
	return step, nil
}

// ListSteps returns all steps of a job ordered by step_number (for the trace
// API and resumption diagnostics).
func (s *PgStore) ListSteps(ctx context.Context, jobID uuid.UUID) ([]JobStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, step_number, step_type, input, output, status, duration_ms, created_at
		 FROM job_steps WHERE job_id = $1 ORDER BY step_number ASC`,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	defer rows.Close()
	var steps []JobStep
	for rows.Next() {
		var st JobStep
		// input/output are nullable JSONB; scan into []byte (database/sql maps
		// NULL -> nil []byte) then convert, since *json.RawMessage is a named type
		// database/sql won't treat as *[]byte.
		var input, output []byte
		if err := rows.Scan(
			&st.ID, &st.JobID, &st.StepNumber, &st.StepType,
			&input, &output, &st.Status, &st.DurationMs, &st.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan step row: %w", err)
		}
		st.Input = json.RawMessage(input)
		st.Output = json.RawMessage(output)
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steps: %w", err)
	}
	return steps, nil
}

// ---------------------------------------------------------------------------
// RenewLease (U2: lease extension as the self-fencing alive-signal)
// ---------------------------------------------------------------------------

// RenewLease pushes a claimed/running job's lease forward while its worker is
// alive, fenced by epoch. This is the heartbeat that also encodes ownership:
// the holder renews every lease/3, so a healthy worker's lease never expires
// (no false reclaim of a long job), and a deposed/zombie worker's renewal
// returns ErrFenced (0 rows, epoch bumped by a reclaim) so it cancels the job
// and stops pinning it. 0 rows ⇒ ErrFenced / ErrNotFound / ErrInvalidTransition.
func (s *PgStore) RenewLease(ctx context.Context, jobID uuid.UUID, epoch int, lease time.Duration) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET lease_expires_at = now() + $2::interval
		 WHERE id = $1 AND lease_epoch = $3 AND status IN ('claimed','running')`,
		jobID, fmt.Sprintf("%d milliseconds", lease.Milliseconds()), epoch,
	)
	if err != nil {
		return fmt.Errorf("renew lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyZeroRows(ctx, jobID, epoch)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

func (s *PgStore) Heartbeat(ctx context.Context, workerID string, hostname string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workers (id, hostname, last_heartbeat, status)
		 VALUES ($1, $2, now(), 'active')
		 ON CONFLICT (id) DO UPDATE
		 SET last_heartbeat = now(), hostname = $2`,
		workerID, hostname,
	)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// classifyZeroRows distinguishes why a fenced write affected zero rows: the
// job is gone (ErrNotFound), the worker's fencing token no longer matches
// (ErrFenced — it was deposed by a reclaim), or the status simply wasn't the
// expected one (ErrInvalidTransition). Only runs on the failure path, so the
// extra round-trip is acceptable.
func (s *PgStore) classifyZeroRows(ctx context.Context, jobID uuid.UUID, epoch int) error {
	var status string
	var leaseEpoch int
	err := s.db.QueryRowContext(ctx,
		`SELECT status, lease_epoch FROM jobs WHERE id = $1`, jobID,
	).Scan(&status, &leaseEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("classify zero rows: %w", err)
	}
	if leaseEpoch != epoch {
		return ErrFenced
	}
	return ErrInvalidTransition
}
