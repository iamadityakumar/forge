package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryBucket implements a thread-safe, token-bucket rate limiter with fractional
// refill and reconcile capabilities (refunds & deficits).
type MemoryBucket struct {
	mu           sync.Mutex
	maxTokens    float64
	tokens       float64
	refillRate   float64 // tokens per second
	lastRefill   time.Time
	clock        Clock
}

// NewMemoryBucket creates a token bucket with capacity maxTokens, refilling over
// refillPeriod (e.g. 1 minute for TPM / RPM).
func NewMemoryBucket(maxTokens int, refillPeriod time.Duration, clock Clock) *MemoryBucket {
	if clock == nil {
		clock = SystemClock{}
	}
	rate := float64(maxTokens) / refillPeriod.Seconds()
	now := clock.Now()
	return &MemoryBucket{
		maxTokens:  float64(maxTokens),
		tokens:     float64(maxTokens),
		refillRate: rate,
		lastRefill: now,
		clock:      clock,
	}
}

func (b *MemoryBucket) refill(now time.Time) {
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillRate
		if b.tokens > b.maxTokens {
			b.tokens = b.maxTokens
		}
		b.lastRefill = now
	}
}

// Reserve attempts to reserve n tokens.
func (b *MemoryBucket) Reserve(ctx context.Context, n int) (Reservation, error) {
	if float64(n) > b.maxTokens {
		return Reservation{}, ErrReservationTooLarge
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()
	b.refill(now)

	req := float64(n)
	if b.tokens >= req {
		b.tokens -= req
		return Reservation{
			Granted:    true,
			TokenCount: n,
			Settle:     b.makeSettle(n),
		}, nil
	}

	deficit := req - b.tokens
	waitSec := deficit / b.refillRate
	waitDur := time.Duration(waitSec * float64(time.Second))

	return Reservation{
		Granted:      false,
		WaitDuration: waitDur,
		TokenCount:   n,
		Settle:       func(actual int) {}, // No-op if reservation was not granted
	}, nil
}

// Wait blocks until n tokens are available or ctx is cancelled.
func (b *MemoryBucket) Wait(ctx context.Context, n int) error {
	for {
		res, err := b.Reserve(ctx, n)
		if err != nil {
			return err
		}
		if res.Granted {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(res.WaitDuration):
		}
	}
}

func (b *MemoryBucket) makeSettle(estimated int) func(actual int) {
	return func(actual int) {
		b.mu.Lock()
		defer b.mu.Unlock()

		now := b.clock.Now()
		b.refill(now)

		diff := estimated - actual
		if diff > 0 {
			// Refund unused tokens
			b.tokens += float64(diff)
			if b.tokens > b.maxTokens {
				b.tokens = b.maxTokens
			}
		} else if diff < 0 {
			// Debit deficit from remaining tokens
			b.tokens += float64(diff) // diff is negative
		}
	}
}