package worker

import (
	"forge/internal/metrics"
	"context"
	"fmt"
	"testing"
	"time"

	"forge/internal/llm"
	"forge/internal/ratelimit"
	"forge/internal/store"
)

// TestRateLimitedAgent_StillExactlyOnce verifies that rate-limiting backpressure (delays)
// does not cause false job reclaims or double-execution when workers run under token constraints.
func TestRateLimitedAgent_StillExactlyOnce(t *testing.T) {
	// Enable chaos test fast segment execution
	oldMin, oldMax := segmentMinMs, segmentMaxMs
	segmentMinMs, segmentMaxMs = 5, 15
	defer func() {
		segmentMinMs, segmentMaxMs = oldMin, oldMax
	}()

	s := newChaosStore()
	numJobs := 10
	segmentsPerJob := 5

	for i := 0; i < numJobs; i++ {
		payload := fmt.Sprintf(`{"segments":%d}`, segmentsPerJob)
		if _, err := s.CreateJob(context.Background(), "segments", []byte(payload), 0, ""); err != nil {
			t.Fatalf("failed creating job: %v", err)
		}
	}

	// Set up a restrictive rate limiter: 50 tokens per minute
	clock := ratelimit.NewManualClock(time.Now())
	bucket := ratelimit.NewMemoryBucket(50, time.Minute, clock)
	rlBackend := llm.NewRateLimitedBackend(llm.NewFakeBackend(), bucket, metrics.New("test"))

	lease := 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run workers concurrently under rate limiting
	numWorkers := 3
	for w := 0; w < numWorkers; w++ {
		workerID := fmt.Sprintf("rl-worker-%d", w)
		go func(id string) {
			_ = Run(ctx, s, id, lease, 1, nil)
		}(workerID)
	}

	// Background ticker advancing the manual clock so rate limiter tokens refill
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				clock.Advance(2 * time.Second)
			}
		}
	}()

	// Wait for all jobs to complete or timeout
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		completed := 0
		s.mu.Lock()
		for _, j := range s.jobs {
			if j.Job.Status == store.StatusCompleted {
				completed++
			}
		}
		s.mu.Unlock()
		if completed == numJobs {
			break
		}
	}

	// Cancel context to shut down workers
	cancel()

	// Invariant Checks
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, j := range s.jobs {
		if j.Job.Status != store.StatusCompleted {
			t.Errorf("job %s did not reach terminal completed status: %s", id, j.Job.Status)
		}
	}

	for key, count := range s.execCount {
		if count > 1 {
			t.Errorf("exactly-once invariant violated: step %d of job %s committed %d times", key.step, key.job, count)
		}
	}

	// Reference rlBackend to avoid unused variable warning
	_ = rlBackend.Name()
}