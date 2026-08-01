package llm

import (
	"context"
	"testing"
	"time"

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