package llm

import (
	"context"
	"testing"
	"time"

	"forge/internal/metrics"
	"forge/internal/ratelimit"
)

type dummyLLM struct {
	promptTokens int
	compTokens   int
}

func (d *dummyLLM) Name() string { return "dummy" }
func (d *dummyLLM) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	return CompleteResponse{
		Content: "ok",
		Usage: Usage{
			PromptTokens:     d.promptTokens,
			CompletionTokens: d.compTokens,
		},
	}, nil
}

func TestEstimateTokens(t *testing.T) {
	req := CompleteRequest{
		Messages: []Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello world!"},
			{Role: "assistant", Content: "Hi there!"},
		},
	}

	est := EstimateTokens(req)
	if est < 500 {
		t.Fatalf("expected minimum estimated tokens >= 500 overhead, got %d", est)
	}
}

func TestRateLimitedBackend_DecoratesAndSettles(t *testing.T) {
	clock := ratelimit.NewManualClock(time.Now())
	bucket := ratelimit.NewMemoryBucket(2000, time.Minute, clock)

	base := &dummyLLM{promptTokens: 150, compTokens: 50} // Total 200 tokens
	rlBackend := NewRateLimitedBackend(base, bucket)

	resp, err := rlBackend.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "Test message"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("expected response content 'ok', got '%s'", resp.Content)
	}
}

func TestRateLimitedBackend_SeedCounters(t *testing.T) {
	m := metrics.New()
	clock := ratelimit.NewManualClock(time.Now())
	bucket := ratelimit.NewMemoryBucket(5000, time.Minute, clock)

	base := &dummyLLM{promptTokens: 150, compTokens: 50}
	rlBackend := NewRateLimitedBackend(base, bucket, m)

	if _, err := rlBackend.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "Test message"}},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// forge_llm_calls_total{backend="dummy"} should be 1.
	if got := m.Value("forge_llm_calls_total", []metrics.Label{{Name: "backend", Value: "dummy"}}); got != 1 {
		t.Fatalf("llm_calls_total = %v, want 1", got)
	}
	// forge_llm_tokens_total{backend="dummy",kind="prompt"} should be 150.
	if got := m.Value("forge_llm_tokens_total", []metrics.Label{{Name: "backend", Value: "dummy"}, {Name: "kind", Value: "prompt"}}); got != 150 {
		t.Fatalf("tokens prompt = %v, want 150", got)
	}
	if got := m.Value("forge_llm_tokens_total", []metrics.Label{{Name: "backend", Value: "dummy"}, {Name: "kind", Value: "completion"}}); got != 50 {
		t.Fatalf("tokens completion = %v, want 50", got)
	}
	// No backpressure wait: wait counters stay 0.
	if got := m.Value("forge_rate_limit_waits_total", nil); got != 0 {
		t.Fatalf("waits_total = %v, want 0", got)
	}
}

func TestRateLimitedBackend_BackpressureCounters(t *testing.T) {
	m := metrics.New()
	// Small bucket with fast refill (rate 1000/s) so forced wait is ~0.5s.
	bucket := ratelimit.NewMemoryBucket(1000, 2*time.Second, nil)
	// Drain the bucket so the decorator's reservation is denied once.
	if _, err := bucket.Reserve(context.Background(), 1000); err != nil {
		t.Fatalf("drain reserve: %v", err)
	}

	base := &dummyLLM{promptTokens: 150, compTokens: 50}
	rlBackend := NewRateLimitedBackend(base, bucket, m)

	start := time.Now()
	if _, err := rlBackend.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "Test message"}},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(start)

	if got := m.Value("forge_rate_limit_waits_total", nil); got != 1 {
		t.Fatalf("waits_total = %v, want 1 (elapsed %v)", got, elapsed)
	}
	if got := m.Value("forge_rate_limit_wait_seconds", nil); got < 0.3 {
		t.Fatalf("wait_seconds = %v, want >= 0.3 (elapsed %v)", got, elapsed)
	}
	// LLM call should still be counted.
	if got := m.Value("forge_llm_calls_total", []metrics.Label{{Name: "backend", Value: "dummy"}}); got != 1 {
		t.Fatalf("llm_calls_total = %v, want 1", got)
	}
}