package sim

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"

	"forge/internal/clock"
	"forge/internal/store"
)

// simStore is a thread-safe in-memory JobStore with proper claiming logic
// for deterministic simulation tests.
type simStore struct {
	mu          sync.Mutex
	jobs        map[uuid.UUID]*simJob
	steps       map[uuid.UUID][]store.JobStep
	claimCursor []uuid.UUID
	clk         clock.Clock
}

type simJob struct {
	store.Job
}

func newSimStore(clk clock.Clock) *simStore {
	return &simStore{
		jobs:        make(map[uuid.UUID]*simJob),
		steps:       make(map[uuid.UUID][]store.JobStep),
		claimCursor: make([]uuid.UUID, 0),
		clk:         clk,
	}
}

func (s *simStore) now() time.Time {
	return s.clk.Now()
}

// CreateJob creates a new job.
func (s *simStore) CreateJob(ctx context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if idempotencyKey != "" {
		for _, j := range s.jobs {
			if j.IdempotencyKey != nil && *j.IdempotencyKey == idempotencyKey {
				return j.Job, nil
			}
		}
	}

	var idemKey *string
	if idempotencyKey != "" {
		idemKey = &idempotencyKey
	}
	j := &simJob{}
	j.ID = uuid.New()
	j.TaskType = taskType
	j.Payload = payload
	j.Status = store.StatusPending
	j.Priority = priority
	j.IdempotencyKey = idemKey
	j.CreatedAt = time.Now().UTC()
	j.MaxAttempts = 3
	s.jobs[j.ID] = j
	s.claimCursor = append(s.claimCursor, j.ID)
	return j.Job, nil
}

func (s *simStore) GetJob(ctx context.Context, id uuid.UUID) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return j.Job, nil
}

func (s *simStore) ListJobs(ctx context.Context, opts store.ListJobsOpts) ([]store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Job
	for _, j := range s.jobs {
		if opts.Status != "" {
			if opts.Status == store.StatusDeadLetter {
				if !j.DeadLetter {
					continue
				}
			} else if j.Status != opts.Status {
				continue
			}
		}
		if opts.TaskType != "" && j.TaskType != opts.TaskType {
			continue
		}
		if opts.WorkerID != "" {
			if j.ClaimedBy == nil || *j.ClaimedBy != opts.WorkerID {
				continue
			}
		}
		if opts.Since != nil && j.CreatedAt.Before(*opts.Since) {
			continue
		}
		out = append(out, j.Job)
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	if opts.Offset > 0 && opts.Offset < len(out) {
		out = out[opts.Offset:]
	} else if opts.Offset >= len(out) {
		out = []store.Job{}
	}
	return out, nil
}

func (s *simStore) CountPendingJobs(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	for _, j := range s.jobs {
		if j.Status == store.StatusPending {
			count++
		}
	}
	return count, nil
}

func (s *simStore) CountJobs(ctx context.Context) (store.JobCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c store.JobCounts
	for _, j := range s.jobs {
		c.Total++
		switch j.Status {
		case store.StatusPending:
			c.Pending++
		case store.StatusClaimed, store.StatusRunning:
			c.Running++
		case store.StatusCompleted:
			c.Completed++
		case store.StatusFailed:
			c.Failed++
		}
		if j.DeadLetter {
			c.DeadLetter++
		}
	}
	return c, nil
}

// ClaimJob implements the proper claiming logic with fencing and SKIP LOCKED semantics.
func (s *simStore) ClaimJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var pick *simJob

	for _, id := range s.claimCursor {
		j := s.jobs[id]
		if j == nil {
			continue
		}
		claimable := false
		switch j.Status {
		case store.StatusPending:
			claimable = true
		case store.StatusClaimed, store.StatusRunning:
			claimable = j.LeaseExpiresAt != nil && j.LeaseExpiresAt.Before(now)
		}
		if !claimable {
			continue
		}
		if j.RunAt != nil && j.RunAt.After(now) {
			continue
		}
		pick = j
		break
	}

	if pick == nil {
		return nil, nil
	}

	pick.Status = store.StatusClaimed
	wid := workerID
	pick.ClaimedBy = &wid
	exp := now.Add(leaseDuration)
	pick.LeaseExpiresAt = &exp
	pick.LeaseEpoch++
	pick.AttemptCount++
	pick.RunAt = nil

	// Return a copy
	jobCopy := pick.Job
	return &jobCopy, nil
}

func (s *simStore) StartJob(ctx context.Context, jobID uuid.UUID, epoch int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	if j.Status != store.StatusClaimed {
		return store.ErrInvalidTransition
	}
	j.Status = store.StatusRunning
	return nil
}

func (s *simStore) CompleteJob(ctx context.Context, jobID uuid.UUID, epoch int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	if j.Status != store.StatusRunning {
		return store.ErrInvalidTransition
	}
	j.Status = store.StatusCompleted
	now := s.now()
	j.CompletedAt = &now
	j.LeaseExpiresAt = nil
	return nil
}

var (
	simBackoffBase = 2 * time.Second
	simBackoffCap  = 5 * time.Minute
)

func simComputeBackoff(attempts int) time.Duration {
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 20 {
		shift = 20
	}
	d := simBackoffBase * (1 << shift)
	if d > simBackoffCap {
		d = simBackoffCap
	}
	// Add small deterministic jitter for sim (0-10ms)
	jitter := time.Duration(rand.Int63n(10)) * time.Millisecond
	if d+jitter > simBackoffCap {
		d = simBackoffCap
	} else {
		d += jitter
	}
	return d
}

func (s *simStore) FailJob(ctx context.Context, jobID uuid.UUID, epoch int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch || j.Status != store.StatusRunning {
		return store.ErrFenced
	}

	reasonStr := reason
	j.ErrorMessage = &reasonStr

	if j.AttemptCount >= j.MaxAttempts {
		j.Status = store.StatusFailed
		j.DeadLetter = true
		now := s.now()
		j.CompletedAt = &now
		j.ClaimedBy = nil
		j.LeaseExpiresAt = nil
		j.RunAt = nil
		return nil
	}

	// Requeue after backoff
	j.Status = store.StatusPending
	j.ClaimedBy = nil
	j.LeaseExpiresAt = nil
	j.LeaseEpoch++
	delay := simComputeBackoff(j.AttemptCount)
	runAt := s.now().Add(delay)
	j.RunAt = &runAt
	return nil
}

func (s *simStore) RecordStep(ctx context.Context, jobID uuid.UUID, epoch int, step store.JobStep) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return uuid.Nil, store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return uuid.Nil, store.ErrFenced
	}
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if step.ID == uuid.Nil {
		step.ID = uuid.New()
	}
	step.JobID = jobID
	step.Status = store.StepCompleted
	step.CreatedAt = s.now()
	s.steps[jobID] = append(s.steps[jobID], step)
	return step.ID, nil
}

func (s *simStore) LastCompletedStep(ctx context.Context, jobID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := s.steps[jobID]
	maxStep := 0
	for _, st := range steps {
		if st.Status == store.StepCompleted && st.StepNumber > maxStep {
			maxStep = st.StepNumber
		}
	}
	return maxStep, nil
}

func (s *simStore) ListSteps(ctx context.Context, jobID uuid.UUID) ([]store.JobStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steps[jobID], nil
}

func (s *simStore) RecordLLMCall(ctx context.Context, call store.LLMCall) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func (s *simStore) ListLLMCalls(ctx context.Context, jobID uuid.UUID) ([]store.LLMCall, error) {
	return nil, nil
}

func (s *simStore) RenewLease(ctx context.Context, jobID uuid.UUID, epoch int, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch || (j.Status != store.StatusClaimed && j.Status != store.StatusRunning) {
		return store.ErrFenced
	}
	exp := s.now().Add(lease)
	j.LeaseExpiresAt = &exp
	return nil
}

func (s *simStore) Heartbeat(ctx context.Context, workerID string, hostname string) error {
	return nil
}

func (s *simStore) CountActiveWorkers(ctx context.Context, within time.Duration) (int, error) {
	return 1, nil
}

func (s *simStore) SetTraceContext(ctx context.Context, jobID uuid.UUID, epoch int, tc json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return store.ErrNotFound
	}
	if j.LeaseEpoch != epoch {
		return store.ErrFenced
	}
	j.TraceContext = tc
	return nil
}

func (s *simStore) Ping(ctx context.Context) error {
	return nil
}

func (s *simStore) Close() error {
	return nil
}

// NewSimStore creates a new in-memory store for simulation tests.
func NewSimStore(clk clock.Clock) store.JobStore {
	return newSimStore(clk)
}