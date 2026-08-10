package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type PgStore struct {
	db *sql.DB
}

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

func (s *PgStore) DB() *sql.DB { return s.db }
func (s *PgStore) Close() error { return s.db.Close() }
func (s *PgStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *PgStore) CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (Job, error) {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	var idemKey *string
	if idempotencyKey != "" {
		idemKey = &idempotencyKey
	}

	query := `
		INSERT INTO jobs (task_type, payload, priority, idempotency_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO UPDATE SET id = jobs.id
		RETURNING id, task_type, payload, status, priority, idempotency_key,
		          claimed_by, lease_expires_at, attempt_count, max_attempts,
		          error_message, created_at, lease_epoch, run_at, completed_at, dead_letter, trace_context
	`

	var job Job
	var tc []byte
	err := s.db.QueryRowContext(ctx, query, taskType, payload, priority, idemKey).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
		&job.LeaseEpoch, &job.RunAt, &job.CompletedAt, &job.DeadLetter, &tc,
	)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	job.TraceContext = json.RawMessage(tc)
	return job, nil
}

func (s *PgStore) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	query := `
		SELECT id, task_type, payload, status, priority, idempotency_key,
		       claimed_by, lease_expires_at, attempt_count, max_attempts,
		       error_message, created_at, lease_epoch, run_at, completed_at, dead_letter, trace_context
		FROM jobs WHERE id = $1
	`
	var job Job
	var tc []byte
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
		&job.LeaseEpoch, &job.RunAt, &job.CompletedAt, &job.DeadLetter, &tc,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	job.TraceContext = json.RawMessage(tc)
	return job, nil
}

func (s *PgStore) ListJobs(ctx context.Context, opts ListJobsOpts) ([]Job, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000 // hard cap
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	// Build dynamic query
	baseQuery := `
		SELECT id, task_type, payload, status, priority, idempotency_key,
		       claimed_by, lease_expires_at, attempt_count, max_attempts,
		       error_message, created_at, lease_epoch, run_at, completed_at, dead_letter, trace_context
		FROM jobs
	`
	whereClauses := []string{}
	args := []any{}
	argIdx := 1

	if opts.Status != "" {
		if opts.Status == StatusDeadLetter {
			whereClauses = append(whereClauses, "dead_letter = true")
		} else {
			whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argIdx))
			args = append(args, opts.Status)
			argIdx++
		}
	}

	if opts.TaskType != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("task_type = $%d", argIdx))
		args = append(args, opts.TaskType)
		argIdx++
	}

	if opts.WorkerID != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("claimed_by = $%d", argIdx))
		args = append(args, opts.WorkerID)
		argIdx++
	}

	if opts.Since != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_at >= $%d", argIdx))
		args = append(args, *opts.Since)
		argIdx++
	}

	if len(whereClauses) > 0 {
		baseQuery += " WHERE " + strings.Join(whereClauses, " AND ")
	}

	baseQuery += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, opts.Limit, opts.Offset)

	return s.scanJobs(ctx, baseQuery, args...)
}

func (s *PgStore) scanJobs(ctx context.Context, query string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		var tc []byte
		if err := rows.Scan(
			&j.ID, &j.TaskType, &j.Payload, &j.Status,
			&j.Priority, &j.IdempotencyKey, &j.ClaimedBy,
			&j.LeaseExpiresAt, &j.AttemptCount, &j.MaxAttempts,
			&j.ErrorMessage, &j.CreatedAt,
			&j.LeaseEpoch, &j.RunAt, &j.CompletedAt, &j.DeadLetter, &tc,
		); err != nil {
			return nil, fmt.Errorf("scan job row: %w", err)
		}
		j.TraceContext = json.RawMessage(tc)
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}
	return jobs, nil
}

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
		          error_message, created_at, lease_epoch, run_at, completed_at, dead_letter, trace_context
	`

	interval := fmt.Sprintf("%d milliseconds", leaseDuration.Milliseconds())

	var job Job
	var tc []byte
	err := s.db.QueryRowContext(ctx, query, workerID, interval).Scan(
		&job.ID, &job.TaskType, &job.Payload, &job.Status,
		&job.Priority, &job.IdempotencyKey, &job.ClaimedBy,
		&job.LeaseExpiresAt, &job.AttemptCount, &job.MaxAttempts,
		&job.ErrorMessage, &job.CreatedAt,
		&job.LeaseEpoch, &job.RunAt, &job.CompletedAt, &job.DeadLetter, &tc,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	job.TraceContext = json.RawMessage(tc)
	return &job, nil
}

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

var (
	backoffBase   = 2 * time.Second
	backoffCap    = 5 * time.Minute
	backoffJitter func() time.Duration = defaultBackoffJitter
)

func defaultBackoffJitter() time.Duration {
	return time.Duration(rand.Int63n(int64(backoffBase) + 1))
}

func computeBackoff(attempts int) time.Duration {
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 20 {
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

func (s *PgStore) RecordStep(ctx context.Context, jobID uuid.UUID, epoch int, step JobStep) (uuid.UUID, error) {
	query := `
		WITH owned AS (
		    SELECT 1 FROM jobs WHERE id = $1 AND lease_epoch = $2 FOR UPDATE
		)
		INSERT INTO job_steps (job_id, step_number, step_type, input, output, status, duration_ms, worker_id)
		SELECT $1, $3, $4, $5, $6, 'completed', $7, NULLIF($8,'') FROM owned
		ON CONFLICT (job_id, step_number) DO UPDATE
		SET output = EXCLUDED.output, status = EXCLUDED.status,
		    duration_ms = EXCLUDED.duration_ms, worker_id = EXCLUDED.worker_id
		RETURNING id
	`
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx, query,
		jobID, epoch, step.StepNumber, step.StepType, step.Input, step.Output, step.DurationMs, step.WorkerID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrFenced
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("record step: %w", err)
	}
	return id, nil
}

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

func (s *PgStore) ListSteps(ctx context.Context, jobID uuid.UUID) ([]JobStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, job_id, step_number, step_type, input, output, status, duration_ms, created_at, worker_id
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
		var input, output []byte
		var workerID sql.NullString
		if err := rows.Scan(
			&st.ID, &st.JobID, &st.StepNumber, &st.StepType,
			&input, &output, &st.Status, &st.DurationMs, &st.CreatedAt, &workerID,
		); err != nil {
			return nil, fmt.Errorf("scan step row: %w", err)
		}
		st.Input = json.RawMessage(input)
		st.Output = json.RawMessage(output)
		if workerID.Valid {
			st.WorkerID = workerID.String
		}
		steps = append(steps, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steps: %w", err)
	}
	return steps, nil
}

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

func (s *PgStore) CountActiveWorkers(ctx context.Context, within time.Duration) (int, error) {
	if within <= 0 {
		within = 30 * time.Second
	}
	interval := fmt.Sprintf("%d milliseconds", within.Milliseconds())
	query := `SELECT COUNT(*) FROM workers WHERE last_heartbeat > now() - $1::interval`
	var count int
	err := s.db.QueryRowContext(ctx, query, interval).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active workers: %w", err)
	}
	return count, nil
}

func (s *PgStore) SetTraceContext(ctx context.Context, jobID uuid.UUID, epoch int, tc json.RawMessage) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET trace_context = $3 WHERE id = $1 AND lease_epoch = $2`,
		jobID, epoch, tc,
	)
	if err != nil {
		return fmt.Errorf("set trace context: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyZeroRows(ctx, jobID, epoch)
	}
	return nil
}

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

func (s *PgStore) CountPendingJobs(ctx context.Context) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM jobs WHERE status = 'pending'`
	err := s.db.QueryRowContext(ctx, query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count pending jobs: %w", err)
	}
	return count, nil
}

func (s *PgStore) CountJobs(ctx context.Context) (JobCounts, error) {
	var c JobCounts
	query := `
		SELECT
			COUNT(*)                                          AS total,
			COUNT(*) FILTER (WHERE status = 'pending')        AS pending,
			COUNT(*) FILTER (WHERE status IN ('claimed','running')) AS running,
			COUNT(*) FILTER (WHERE status = 'completed')      AS completed,
			COUNT(*) FILTER (WHERE status = 'failed')         AS failed,
			COUNT(*) FILTER (WHERE dead_letter = true)        AS dead_letter
		FROM jobs
	`
	err := s.db.QueryRowContext(ctx, query).Scan(&c.Total, &c.Pending, &c.Running, &c.Completed, &c.Failed, &c.DeadLetter)
	if err != nil {
		return JobCounts{}, fmt.Errorf("count jobs: %w", err)
	}
	return c, nil
}

func (s *PgStore) RecordLLMCall(ctx context.Context, call LLMCall) (uuid.UUID, error) {
	if call.ID == uuid.Nil {
		call.ID = uuid.New()
	}
	query := `
		INSERT INTO llm_calls (
			id, job_id, worker_id, backend, prompt_tokens, completion_tokens,
			estimated_tokens, latency_ms, error, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		RETURNING id
	`
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx, query,
		call.ID, call.JobID, call.WorkerID, call.Backend,
		call.PromptTokens, call.CompletionTokens, call.EstimatedTokens,
		call.LatencyMs, call.Error,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("record llm call: %w", err)
	}
	return id, nil
}

func (s *PgStore) ListLLMCalls(ctx context.Context, jobID uuid.UUID) ([]LLMCall, error) {
	query := `
		SELECT id, job_id, worker_id, backend, prompt_tokens, completion_tokens,
		       estimated_tokens, latency_ms, error, created_at
		FROM llm_calls
		WHERE job_id = $1
		ORDER BY created_at ASC
	`
	rows, err := s.db.QueryContext(ctx, query, jobID)
	if err != nil {
		return nil, fmt.Errorf("list llm calls: %w", err)
	}
	defer rows.Close()

	var calls []LLMCall
	for rows.Next() {
		var c LLMCall
		if err := rows.Scan(
			&c.ID, &c.JobID, &c.WorkerID, &c.Backend, &c.PromptTokens,
			&c.CompletionTokens, &c.EstimatedTokens, &c.LatencyMs, &c.Error, &c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan llm call: %w", err)
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate llm calls: %w", err)
	}
	if calls == nil {
		calls = []LLMCall{}
	}
	return calls, nil
}
