package llm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"forge/internal/ratelimit"
)

// RateLimitedBackend decorates an LLMBackend with rate limiting, reserving estimated prompt cost,
// logging backpressure WARN when waiting, and settling actual token usage post-call.
type RateLimitedBackend struct {
	backend LLMBackend
	limiter ratelimit.Limiter
}

// NewRateLimitedBackend wraps an underlying LLMBackend with the given ratelimit.Limiter.
func NewRateLimitedBackend(backend LLMBackend, limiter ratelimit.Limiter) *RateLimitedBackend {
	return &RateLimitedBackend{
		backend: backend,
		limiter: limiter,
	}
}

func (r *RateLimitedBackend) Name() string {
	return r.backend.Name()
}

// Complete estimates tokens, reserves capacity, calls the provider, and settles actual usage.
func (r *RateLimitedBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	if r.limiter == nil {
		return r.backend.Complete(ctx, req)
	}

	est := EstimateTokens(req)

	// Attempt reservation
	res, err := r.limiter.Reserve(ctx, est)
	if err != nil {
		return CompleteResponse{}, fmt.Errorf("rate limiter reserve: %w", err)
	}

	if !res.Granted {
		slog.Warn("llm rate limit backpressure",
			"estimated_tokens", est,
			"wait_ms", res.WaitDuration.Milliseconds(),
		)
		// Wait until tokens become available or ctx is cancelled
		t0 := time.Now()
		select {
		case <-ctx.Done():
			return CompleteResponse{}, ctx.Err()
		case <-time.After(res.WaitDuration):
			slog.Info("rate limit wait complete", "waited_ms", time.Since(t0).Milliseconds())
		}

		// Re-attempt reservation after wait
		res, err = r.limiter.Reserve(ctx, est)
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("rate limiter reserve post-wait: %w", err)
		}
		if !res.Granted {
			// If still not granted, fall back to Wait helper
			if err := r.limiter.Wait(ctx, est); err != nil {
				return CompleteResponse{}, fmt.Errorf("rate limiter wait: %w", err)
			}
		}
	}

	// Call underlying LLM backend
	resp, err := r.backend.Complete(ctx, req)
	if err != nil {
		// Refund estimated tokens on provider error
		if res.Settle != nil {
			res.Settle(0)
		}
		return CompleteResponse{}, err
	}

	// Settle actual tokens consumed
	if res.Settle != nil {
		actual := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		res.Settle(actual)
	}

	return resp, nil
}