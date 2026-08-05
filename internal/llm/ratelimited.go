package llm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"forge/internal/metrics"
	"forge/internal/ratelimit"
	"forge/internal/trace"
)

// RateLimitedBackend decorates an LLMBackend with rate limiting, reserving estimated prompt cost,
// logging backpressure WARN when waiting, and settling actual token usage post-call.
type RateLimitedBackend struct {
	backend      LLMBackend
	limiter      ratelimit.Limiter
	metricsStore *metrics.Metrics
}

// NewRateLimitedBackend wraps an underlying LLMBackend with the given ratelimit.Limiter.
// A *metrics.Metrics store is required for Prometheus metrics.
func NewRateLimitedBackend(backend LLMBackend, limiter ratelimit.Limiter, m *metrics.Metrics) *RateLimitedBackend {
	return &RateLimitedBackend{
		backend:      backend,
		limiter:      limiter,
		metricsStore: m,
	}
}

func (r *RateLimitedBackend) Name() string {
	return r.backend.Name()
}

// completeWithSpan wraps the provider call in a "llm.complete" span that is a
// child of the caller's span (the agent's current step span). The span context
// flows into the provider HTTP request via trace.InjectW3C in the backends, so
// a traced provider would see the same trace. No-op (non-recording) when no
// trace provider is set up.
func (r *RateLimitedBackend) completeWithSpan(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	tracer := trace.NewTracer(r.backend.Name())
	callCtx, span := tracer.StartSpan(ctx, "llm.complete",
		trace.Attribute{Key: "backend", Value: r.backend.Name()},
	)
	defer span.End()

	resp, err := r.backend.Complete(callCtx, req)
	if err != nil {
		span.SetStatus("error", err)
	} else {
		span.SetStatus("ok", nil)
	}
	return resp, err
}

// Complete estimates tokens, reserves capacity, calls the provider, and settles actual usage.
func (r *RateLimitedBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	m := r.metricsStore
	backendName := r.backend.Name()

	if r.limiter == nil {
		if m != nil {
			m.LLMCalls.WithLabelValues(backendName).Inc()
		}
		t0 := time.Now()
		resp, err := r.completeWithSpan(ctx, req)
		dur := time.Since(t0)
		if m != nil {
			m.LLMDuration.WithLabelValues(backendName).Observe(dur.Seconds())
		}
		if err != nil {
			if m != nil {
				m.LLMErrors.WithLabelValues(backendName, ClassifyError(err)).Inc()
			}
			return CompleteResponse{}, err
		}
		if m != nil {
			m.LLMTokens.WithLabelValues(backendName, "prompt").Add(float64(resp.Usage.PromptTokens))
			m.LLMTokens.WithLabelValues(backendName, "completion").Add(float64(resp.Usage.CompletionTokens))
		}
		return resp, nil
	}

	est := EstimateTokens(req)

	// Attempt reservation
	res, err := r.limiter.Reserve(ctx, est)
	if err != nil {
		return CompleteResponse{}, fmt.Errorf("rate limiter reserve: %w", err)
	}

	limiterName := "memory"
	if !res.Granted {
		slog.Warn("llm rate limit backpressure",
			"estimated_tokens", est,
			"wait_ms", res.WaitDuration.Milliseconds(),
		)
		t0 := time.Now()
		select {
		case <-ctx.Done():
			return CompleteResponse{}, ctx.Err()
		case <-time.After(res.WaitDuration):
			waited := time.Since(t0)
			if m != nil {
				m.RateLimitWaits.WithLabelValues(limiterName).Inc()
				m.RateLimitWaitTime.WithLabelValues(limiterName).Observe(waited.Seconds())
			}
			slog.Info("rate limit wait complete", "waited_ms", waited.Milliseconds())
		}

		res, err = r.limiter.Reserve(ctx, est)
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("rate limiter reserve post-wait: %w", err)
		}
		if !res.Granted {
			if err := r.limiter.Wait(ctx, est); err != nil {
				return CompleteResponse{}, fmt.Errorf("rate limiter wait: %w", err)
			}
		}
	}

	// Call underlying LLM backend
	if m != nil {
		m.LLMCalls.WithLabelValues(backendName).Inc()
	}
	callStart := time.Now()
	resp, err := r.completeWithSpan(ctx, req)
	callDur := time.Since(callStart)
	if m != nil {
		m.LLMDuration.WithLabelValues(backendName).Observe(callDur.Seconds())
	}

	if err != nil {
		if m != nil {
			m.LLMErrors.WithLabelValues(backendName, ClassifyError(err)).Inc()
		}
		if res.Settle != nil {
			res.Settle(0)
		}
		return CompleteResponse{}, err
	}

	// Metrics: record actual token usage.
	if m != nil {
		m.LLMTokens.WithLabelValues(backendName, "prompt").Add(float64(resp.Usage.PromptTokens))
		m.LLMTokens.WithLabelValues(backendName, "completion").Add(float64(resp.Usage.CompletionTokens))
	}

	if res.Settle != nil {
		actual := resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		res.Settle(actual)
	}

	return resp, nil
}