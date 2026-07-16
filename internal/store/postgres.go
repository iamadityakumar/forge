package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		          error_message, created_at
	`

	var job Job
	err := s.db.QueryRowContext(ctx, query, taskType, payload, priority, idemKey).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
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
		       error_message, created_at
		FROM jobs WHERE id = $1
	`
	var job Job
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
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

func (s *PgStore) ListJobs(ctx context.Context, status string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `
		SELECT id, task_type, payload, status, priority, idempotency_key,
		       claimed_by, lease_expires_at, attempt_count, max_attempts,
		       error_message, created_at
		FROM jobs
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2
	`

	rows, err := s.db.QueryContext(ctx, query, status, limit)
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
// ClaimJob — the core engineering artifact (SKIP LOCKED)
// ---------------------------------------------------------------------------

func (s *PgStore) ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*Job, error) {
	// The subselect finds the next claimable job:
	//   - pending jobs (never claimed), OR
	//   - claimed jobs whose lease has expired (crashed worker)
	// FOR UPDATE SKIP LOCKED ensures two workers never claim the same row.
	query := `
		UPDATE jobs
		SET status       = 'claimed',
		    claimed_by   = $1,
		    lease_expires_at = now() + $2::interval,
		    attempt_count = attempt_count + 1
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending'
			   OR (status = 'claimed' AND lease_expires_at < now())
			ORDER BY priority DESC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, task_type, payload, status, priority, idempotency_key,
		          claimed_by, lease_expires_at, attempt_count, max_attempts,
		          error_message, created_at
	`

	// Format lease duration as a Postgres interval (e.g. "120 seconds").
	interval := fmt.Sprintf("%d seconds", int(leaseDuration.Seconds()))

	var job Job
	err := s.db.QueryRowContext(ctx, query, workerID, interval).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
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
// State transitions
// ---------------------------------------------------------------------------

// StartJob transitions a job from claimed → running.
func (s *PgStore) StartJob(ctx context.Context, jobID uuid.UUID) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'running' WHERE id = $1 AND status = 'claimed'`,
		jobID,
	)
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}
	return s.expectOneRow(res, "start job")
}

// CompleteJob transitions a job from running → completed.
func (s *PgStore) CompleteJob(ctx context.Context, jobID uuid.UUID) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'completed' WHERE id = $1 AND status = 'running'`,
		jobID,
	)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return s.expectOneRow(res, "complete job")
}

// FailJob transitions a job from running → failed and records the error reason.
func (s *PgStore) FailJob(ctx context.Context, jobID uuid.UUID, reason string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'failed', error_message = $2 WHERE id = $1 AND status = 'running'`,
		jobID, reason,
	)
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	return s.expectOneRow(res, "fail job")
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

func (s *PgStore) expectOneRow(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}
