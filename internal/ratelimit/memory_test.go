package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestMemoryBucket_ReserveAndSettle(t *testing.T) {
	now := time.Now()
	clock := NewManualClock(now)

	// 100 tokens max, refilling over 1 minute (100 tokens / 60s ≈ 1.667 tokens/sec)
	bucket := NewMemoryBucket(100, time.Minute, clock)

	// 1. Initial reservation granted
	res, err := bucket.Reserve(context.Background(), 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Granted {
		t.Fatalf("expected reservation to be granted")
	}

	// 2. Reserve another 60 tokens (only 50 tokens remaining) -> Not granted
	res2, err := bucket.Reserve(context.Background(), 60)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.Granted {
		t.Fatalf("expected reservation to be denied due to capacity")
	}
	if res2.WaitDuration <= 0 {
		t.Fatalf("expected positive wait duration, got %v", res2.WaitDuration)
	}

	// 3. Settle original reservation with actual usage = 30 (refund 20 tokens)
	res.Settle(30)

	// 4. Reserve 60 tokens again (50 remaining + 20 refund = 70 tokens) -> Granted
	res3, err := bucket.Reserve(context.Background(), 60)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res3.Granted {
		t.Fatalf("expected reservation to be granted after refund")
	}
}

func TestMemoryBucket_RefillOverTime(t *testing.T) {
	now := time.Now()
	clock := NewManualClock(now)

	bucket := NewMemoryBucket(60, time.Minute, clock) // 1 token per second

	res, _ := bucket.Reserve(context.Background(), 60)
	if !res.Granted {
		t.Fatalf("expected initial reservation granted")
	}

	// Bucket empty now
	res2, _ := bucket.Reserve(context.Background(), 10)
	if res2.Granted {
		t.Fatalf("expected denied when empty")
	}

	// Advance clock by 30 seconds -> 30 tokens refilled
	clock.Advance(30 * time.Second)

	res3, _ := bucket.Reserve(context.Background(), 25)
	if !res3.Granted {
		t.Fatalf("expected granted after 30s refill")
	}
}

func TestMultiLimiter_TPM_RPM(t *testing.T) {
	now := time.Now()
	clock := NewManualClock(now)

	tpm := NewMemoryBucket(1000, time.Minute, clock)
	rpm := NewMemoryBucket(2, time.Minute, clock)

	multi := NewMultiLimiter(tpm, rpm)

	// 1st request -> ok
	res1, err := multi.Reserve(context.Background(), 100)
	if err != nil || !res1.Granted {
		t.Fatalf("expected 1st request granted")
	}

	// 2nd request -> ok
	res2, err := multi.Reserve(context.Background(), 100)
	if err != nil || !res2.Granted {
		t.Fatalf("expected 2nd request granted")
	}

	// 3rd request -> denied by RPM (capacity 2)
	res3, err := multi.Reserve(context.Background(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res3.Granted {
		t.Fatalf("expected 3rd request to be denied by RPM limit")
	}
}