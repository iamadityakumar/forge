package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"forge/internal/clock"
	"forge/internal/store"
)

// Sim is a lightweight helper for deterministic simulation tests.
// It provides a ManualClock and in-memory store for testing time-dependent
// behavior without real goroutines.
type Sim struct {
	clk   *clock.ManualClock
	store store.JobStore
}

// Advance advances the virtual clock and returns the new time.
func (s *Sim) Advance(d time.Duration) time.Time {
	s.clk.Advance(d)
	return s.clk.Now()
}

// NewJob creates a job with the given task type and segments.
func (s *Sim) NewJob(taskType string, segments int) uuid.UUID {
	payload := fmt.Sprintf(`{"segments":%d}`, segments)
	ctx := context.Background()
	j, err := s.store.CreateJob(ctx, taskType, []byte(payload), 0, "")
	if err != nil {
		panic(fmt.Sprintf("create job: %v", err))
	}
	return j.ID
}

// GetJob fetches a job from the store.
func (s *Sim) GetJob(jobID uuid.UUID) *store.Job {
	job, err := s.store.GetJob(context.Background(), jobID)
	if err != nil {
		return nil
	}
	return &job
}

// NewSim creates a new Sim with a ManualClock and in-memory store.
func NewSim(clk *clock.ManualClock, s store.JobStore) *Sim {
	return &Sim{clk: clk, store: s}
}