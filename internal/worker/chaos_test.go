package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"forge/internal/store"
)

// chaos_test.go — the project's thesis expressed as a passing test instead of a
// story (U7). Instead of one kill -9 + eyeballing a trace, it runs a fleet of
// workers against an in-process store that faithfully emulates fencing +
// SKIP LOCKED claim + reclaim-running + lease expiry, while a seeded
// pseudo-random killer cancels a random worker's context (a kill -9 simulation)
// with a replacement spawned so the fleet stays alive. After the dust settles
// it asserts three invariants:
//
//  1. Liveness  — every submitted job reaches a terminal state (completed or
//     dead_letter); none stuck forever.
//  2. Safety     — the headline: no step is executed more than once. A per
//     (job,step) counter incremented on every committed checkpoint stays ≤ 1,
//     and the completed steps per job are exactly {1..K} with no gaps.
//  3. No faults  — no panics and (under go test -race) no data races.
//
// Run repeatedly to catch timing-dependent exactly-once regressions:
//
//	go test -race -count=5 ./internal/worker/...
//
// The fake store is the only "oracle" here — if its fencing emulation were
// wrong, the test would prove nothing. It mirrors postgres.go's invariants
// (ClaimJob bumps lease_epoch and only reclaims pending ∪ expired
// claimed/running; RecordStep is fenced by epoch and idempotent; FailJob
// requeues with backoff or dead-letters). The counter than makes exactly-once
// *observable*: under correct fencing+resume a given (job,step) is committed at
// most once; remove the epoch fence and the counter would exceed one.

// chaosStore is an in-memory JobStore emulating the Postgres fencing/claim
// semantics for the chaos test. All state is guarded by one mutex, so the -race
// detector sees any unsynchronized access (real or fake).
type chaosStore struct {
	mu sync.Mutex

	jobs      map[uuid.UUID]*chaosJob
	steps     map[stepKey]*store.JobStep // checkpoints, like job_steps
	execCount map[stepKey]int            // the exactly-once oracle: committed-step count

	claimCursor []uuid.UUID // iteration order for deterministic "next claimable" selection
}

type stepKey struct {
	job  uuid.UUID
	step int
}

type chaosJob struct {
	store.Job
}

func newChaosStore() *chaosStore {
	return &chaosStore{
		jobs:      make(map[uuid.UUID]*chaosJob),
		steps:     make(map[stepKey]*store.JobStep),
		execCount: make(map[stepKey]int),
	}
}

func (s *chaosStore) now() time.Time { return time.Now() }

func (s *chaosStore) CreateJob(_ context.Context, taskType string, payload json.RawMessage, priority int, idempotencyKey string) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := &chaosJob{}
	j.ID = uuid.New()
	j.TaskType = taskType
	j.Payload = payload
	j.Status = store.StatusPending
	j.Priority = priority
	j.MaxAttempts = 3
	j.CreatedAt = s.now()
	j.LeaseEpoch = 0
	if idempotencyKey != "" {
		k := idempotencyKey
		j.IdempotencyKey = &k
	}
	s.jobs[j.ID] = j
	s.claimCursor = append(s.claimCursor, j.ID)
	return j.Job, nil
}

func (s *chaosStore) GetJob(_ context.Context, id uuid.UUID) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return j.Job, nil
}

func (s *chaosStore) ListJobs(_ context.Context, status string, _ int) ([]store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Job
	for _, j := range s.jobs {
		if status == "" || j.Status == status {
			out = append(out, j.Job)
		}
	}
	return out, nil
}

// ClaimJob emulates postgres.go: pick a pending job, OR a claimed/running job
// whose lease has expired (reclaim), AND whose run_at gate has elapsed; mint a
// fresh fencing token (lease_epoch+1); set claimed + lease. Returns nil,nil when
// nothing is claimable. SKIP LOCKED is emulated by holding the mutex across the
// pick+update (two workers never claim the same row).
func (s *chaosStore) ClaimJob(_ context.Context, workerID string, lease time.Duration) (*store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var pick *chaosJob
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
		if j.RunAt != nil && j.RunAt.After(now) { // run_at gate (backoff delay)
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
	exp := now.Add(lease)
	pick.LeaseExpiresAt = &exp
	pick.LeaseEpoch++
	pick.AttemptCount++
	pick.RunAt = nil
	j := pick.Job
	return &j, nil
}

func (s *chaosStore) StartJob(_ context.Context, jobID uuid.UUID, epoch int) error {
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

func (s *chaosStore) CompleteJob(_ context.Context, jobID uuid.UUID, epoch int) error {
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
	c := s.now()
	j.CompletedAt = &c
	j.LeaseExpiresAt = nil
	return nil
}

// FailJob emulates the requeue-vs-dead-letter branch (U5), fenced by epoch.
func (s *chaosStore) FailJob(_ context.Context, jobID uuid.UUID, epoch int, reason string) error {
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
		c := s.now()
		j.CompletedAt = &c
		j.ClaimedBy = nil
		j.LeaseExpiresAt = nil
		j.RunAt = nil
		return nil
	}
	// Requeue after backoff (a short, deterministic delay for the test).
	j.Status = store.StatusPending
	j.ClaimedBy = nil
	j.LeaseExpiresAt = nil
	j.LeaseEpoch++
	ra := s.now().Add(chaosBackoff(j.AttemptCount))
	j.RunAt = &ra
	return nil
}

// RecordStep emulates the fenced, idempotent checkpoint CTE. It is the
// exactly-once oracle: execCount[(job,step)] is bumped on EVERY successful
// commit (including an ON-CONFLICT re-record), so a correct system keeps it ≤ 1.
func (s *chaosStore) RecordStep(ctx context.Context, jobID uuid.UUID, epoch int, step store.JobStep) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return uuid.Nil, store.ErrNotFound
	}
	// Fence: only the current epoch holder may checkpoint.
	if j.LeaseEpoch != epoch {
		return uuid.Nil, store.ErrFenced
	}
	// Respect cancellation (a kill -9 mid-record aborts the commit).
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	key := stepKey{job: jobID, step: step.StepNumber}
	step.Status = store.StepCompleted
	step.JobID = jobID
	if step.ID == uuid.Nil {
		step.ID = uuid.New()
	}
	step.CreatedAt = s.now()
	s.steps[key] = &step
	s.execCount[key]++ // bumped on every commit, re-record included
	return step.ID, nil
}

func (s *chaosStore) LastCompletedStep(_ context.Context, jobID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	max := 0
	for k, st := range s.steps {
		if k.job != jobID || st.Status != store.StepCompleted {
			continue
		}
		if k.step > max {
			max = k.step
		}
	}
	return max, nil
}

func (s *chaosStore) ListSteps(_ context.Context, jobID uuid.UUID) ([]store.JobStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.JobStep
	// ordered by step_number
	tmp := make(map[int]*store.JobStep)
	maxN := 0
	for k, st := range s.steps {
		if k.job != jobID {
			continue
		}
		tmp[k.step] = st
		if k.step > maxN {
			maxN = k.step
		}
	}
	for i := 1; i <= maxN; i++ {
		if st, ok := tmp[i]; ok {
			out = append(out, *st)
		}
	}
	return out, nil
}

func (s *chaosStore) RenewLease(_ context.Context, jobID uuid.UUID, epoch int, lease time.Duration) error {
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

func (s *chaosStore) Heartbeat(_ context.Context, _ string, _ string) error { return nil }
func (s *chaosStore) Ping(_ context.Context) error                          { return nil }
func (s *chaosStore) Close() error                                          { return nil }

// execCounts returns a copy of the exactly-once counter for assertions.
func (s *chaosStore) execCounts() map[stepKey]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[stepKey]int, len(s.execCount))
	for k, v := range s.execCount {
		out[k] = v
	}
	return out
}

// allTerminal reports whether every job is completed or dead-lettered.
func (s *chaosStore) allTerminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.Status != store.StatusCompleted && !j.DeadLetter {
			return false
		}
	}
	return true
}

// jobStat returns a job's status + dead-letter flag.
func (s *chaosStore) jobStat(id uuid.UUID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	return j.Status, j.DeadLetter
}

// chaosBackoff is a short, test-friendly requeue delay (keeps the chaos test
// fast; the run_at gate still exercises the backoff mechanism).
func chaosBackoff(attempts int) time.Duration {
	d := 10 * time.Millisecond * time.Duration(1<<uint(attempts-1))
	if d > 80*time.Millisecond {
		d = 80 * time.Millisecond
	}
	return d
}

// TestChaosRecoveryKillsExactlyOnce is the U7 invariant test. It is the single
// most important assertion in the repo: under repeated kills, exactly-once
// step execution and liveness both hold.
func TestChaosRecoveryKillsExactlyOnce(t *testing.T) {
	fs := newChaosStore()

	// Segment work is sized so a job clearly outlives one lease window (this
	// exercises lease renewal *and* leaves jobs genuinely in-flight when the
	// reaper kills — the 0.03s "free pass" you get with 1ms segments proves
	// nothing). Still small enough for -race -count N to be fast.
	prevMin, prevMax := segmentMinMs, segmentMaxMs
	segmentMinMs, segmentMaxMs = 12, 30
	t.Cleanup(func() { segmentMinMs, segmentMaxMs = prevMin, prevMax })

	// Seed: default fixed for reproducibility, but CHAOS_SEED=unixnano (or any
	// int) varies it across `-count` runs so the fuzz surface widens.
	seed := int64(0xC4A05)
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		if v, err := strconv.ParseInt(s, 0, 64); err == nil {
			seed = v
		}
	}
	rng := rand.New(rand.NewSource(seed)) // #nosec G404 — deterministic test RNG

	const (
		numJobs        = 24
		segmentsPerJob = 8
		numWorkers     = 4
		concurrency    = 2 // exercise U6 bounded concurrency
		maxKills       = 200
		lease          = 150 * time.Millisecond // short; reclaim resumes an orphan ~150ms after a kill
	)

	// Submit jobs.
	jobIDs := make([]uuid.UUID, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		payload, _ := json.Marshal(map[string]int{"segments": segmentsPerJob})
		j, err := fs.CreateJob(context.Background(), "segments", payload, 0, "")
		if err != nil {
			t.Fatalf("create job %d: %v", i, err)
		}
		jobIDs = append(jobIDs, j.ID)
	}

	// Root context for the fleet; cancelling it stops every worker.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	var (
		runWG    sync.WaitGroup
		workerMu sync.Mutex
		// workers[slot] is the cancel func of the worker currently running in
		// slot `slot`; cancelling it simulates a kill -9. Pre-sized to numWorkers
		// so startWorker can assign by index.
		workers  = make([]context.CancelFunc, numWorkers)
		spawnSeq int
	)
	// Spawn a worker in slot i (cancelling the slot kills it; startWorker may
	// be called again to replace a killed worker in the same slot).
	startWorker := func(slot int) {
		wCtx, wCancel := context.WithCancel(rootCtx)
		workerMu.Lock()
		workers[slot] = wCancel
		spawnSeq++
		name := fmt.Sprintf("w%d-%d", slot, spawnSeq)
		workerMu.Unlock()
		runWG.Add(1)
		go func() {
			defer runWG.Done()
			_ = Run(wCtx, fs, name, lease, concurrency)
		}()
	}
	for i := 0; i < numWorkers; i++ {
		startWorker(i)
	}

	// Reaper: seeded killer that cancels a random worker's context (kill -9
	// sim), then respawns a replacement so the fleet stays alive. Stops once all
	// jobs are terminal (or the test deadline passes).
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		var kills int
		for {
			if kills >= maxKills {
				return
			}
			// jittered inter-kill delay, seeded. Frequent enough that jobs are
			// repeatedly killed mid-flight, not just before/after they run.
			delay := time.Duration(5+rng.Intn(20)) * time.Millisecond
			select {
			case <-time.After(delay):
			case <-rootCtx.Done():
				return
			}
			if fs.allTerminal() {
				return
			}
			workerMu.Lock()
			slot := rng.Intn(len(workers))
			kill := workers[slot]
			workerMu.Unlock()
			if kill != nil {
				kill()            // kill -9 the worker in this slot
				startWorker(slot) // immediately respawn a replacement
			}
			kills++
		}
	}()

	// Wait for liveness: every job terminal within a generous deadline (jobs now
	// run hundreds of ms each, with requeue backoff adding to the worst case).
	deadline := time.Now().Add(60 * time.Second)
	for !fs.allTerminal() {
		if time.Now().After(deadline) {
			// Diagnostics: which jobs are stuck, and where.
			rootCancel()
			<-reaperDone
			runWG.Wait()
			var stuck []string
			for _, id := range jobIDs {
				st, dl := fs.jobStat(id)
				if st != store.StatusCompleted && !dl {
					stuck = append(stuck, fmt.Sprintf("%s=%s", id, st))
				}
			}
			t.Fatalf("liveness violated: jobs not terminal within deadline: %v (seed=0x%X)", stuck, seed)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stop the fleet and wait for every worker + in-flight job to unwind
	// (structured concurrency; also gives the -race detector a clean point).
	rootCancel()
	<-reaperDone
	runWG.Wait()

	// ----- Invariant assertions -----

	// (1) Liveness: every job is completed or dead-lettered.
	for _, id := range jobIDs {
		st, dl := fs.jobStat(id)
		if st != store.StatusCompleted && !dl {
			t.Errorf("job %s not terminal: status=%s dead_letter=%v", id, st, dl)
		}
	}

	// (2) Safety: no step executed more than once (the headline), and per job
	// the completed steps are exactly {1..K} with no gaps.
	counts := fs.execCounts()
	for key, c := range counts {
		if c > 1 {
			t.Errorf("SAFETY VIOLATION: step (job=%s, step=%d) committed %d times — not exactly-once (seed=0x%X)",
				key.job, key.step, c, seed)
		}
	}
	for _, id := range jobIDs {
		steps, err := fs.ListSteps(context.Background(), id)
		if err != nil {
			t.Fatalf("list steps %s: %v", id, err)
		}
		seen := make(map[int]bool, segmentsPerJob)
		for _, st := range steps {
			if st.Status != store.StepCompleted {
				t.Errorf("job %s step %d status %q, want completed", id, st.StepNumber, st.Status)
			}
			if seen[st.StepNumber] {
				t.Errorf("job %s step %d appears twice in trace — not exactly-once", id, st.StepNumber)
			}
			seen[st.StepNumber] = true
		}
		var missing []int
		for i := 1; i <= segmentsPerJob; i++ {
			if !seen[i] {
				missing = append(missing, i)
			}
		}
		// A dead-lettered job may legitimately have fewer-than-K steps; only
		// completed (and not-dead-lettered) jobs must have the full 1..K set.
		st, dl := fs.jobStat(id)
		if st == store.StatusCompleted && !dl {
			if len(missing) > 0 {
				t.Errorf("job %s completed with missing steps %v — recovery left gaps (seed=0x%X)", id, missing, seed)
			}
		}
	}

	// (3) No-faults: reaching here means no panic; -race mode surfaces data
	// races as failures automatically. Re-affirm with an error-free summary.
	if !t.Failed() {
		t.Logf("PASS: %d jobs × %d segments, %d-worker fleet (concurrency=%d), all terminal, all steps exactly-once (seed=0x%X)",
			numJobs, segmentsPerJob, numWorkers, concurrency, seed)
	}
}

// Compile-time assertion that chaosStore satisfies store.JobStore.
var _ store.JobStore = (*chaosStore)(nil)
