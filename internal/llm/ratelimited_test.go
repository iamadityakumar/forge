package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forge/internal/metrics"
	"forge/internal/ratelimit"
	"forge/internal/trace"
)

type dummyLLM struct {
	name      string
	latency   time.Duration
	usage     Usage
	err       error
	callCount int
}

func (d *dummyLLM) Name() string { return d.name }

func (d *dummyLLM) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	d.callCount++
	if d.latency > 0 {
		select {
		case <-time.After(d.latency):
		case <-ctx.Done():
			return CompleteResponse{}, ctx.Err()
		}
	}
	if d.err != nil {
		return CompleteResponse{}, d.err
	}
	return CompleteResponse{
		Content: fmt.Sprintf("dummy response %d", d.callCount),
		Usage:   d.usage,
	}, nil
}

func TestEstimateTokens(t *testing.T) {
	req := CompleteRequest{
		Messages: []Message{{Role: "user", Content: "Hello world"}},
	}
	est := EstimateTokens(req)
	if est < 500 {
		t.Errorf("EstimateTokens() = %d, want >= 500", est)
	}

	emptyEst := EstimateTokens(CompleteRequest{})
	if emptyEst < 100 {
		t.Errorf("EstimateTokens(empty) = %d, want >= 100", emptyEst)
	}
}

func TestRateLimitedBackend_DecoratesAndSettles(t *testing.T) {
	clock := ratelimit.NewManualClock(time.Now())
	bucket := ratelimit.NewMemoryBucket(1000, time.Minute, clock)
	backend := &dummyLLM{name: "test", latency: 0, usage: Usage{PromptTokens: 10, CompletionTokens: 5}}
	m := metrics.New("test")

	rl := NewRateLimitedBackend(backend, bucket, m)

	ctx := context.Background()
	resp, err := rl.Complete(ctx, CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Error("expected non-empty response")
	}
}

func TestRateLimitedBackend_SeedCounters(t *testing.T) {
	clock := ratelimit.NewManualClock(time.Now())
	bucket := ratelimit.NewMemoryBucket(1000, time.Minute, clock)
	backend := &dummyLLM{name: "fake", latency: 0, usage: Usage{PromptTokens: 50, CompletionTokens: 20}}
	m := metrics.New("test")

	rl := NewRateLimitedBackend(backend, bucket, m)

	ctx := context.Background()
	_, err := rl.Complete(ctx, CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	handler := m.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`test_llm_calls_total{backend="fake"} 1`,
		`test_llm_tokens_total{backend="fake",kind="prompt"} 50`,
		`test_llm_tokens_total{backend="fake",kind="completion"} 20`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in metrics:\n%s", want, body)
		}
	}
}

func TestRateLimitedBackend_BackpressureCounters(t *testing.T) {
	// SystemClock so refill happens with real wall-clock time
	bucket := ratelimit.NewMemoryBucket(504, 10*time.Millisecond, ratelimit.SystemClock{})
	backend := &dummyLLM{name: "fake", latency: 0, usage: Usage{PromptTokens: 300, CompletionTokens: 204}}
	m := metrics.New("test")

	rl := NewRateLimitedBackend(backend, bucket, m)

	ctx := context.Background()

	// First call consumes all 504 tokens
	_, err := rl.Complete(ctx, CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}

	// Second call needs ~503 tokens, bucket has 0 -> Granted: false, wait ~10ms
	_, err = rl.Complete(ctx, CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello again"}},
	})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}

	handler := m.Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`test_llm_calls_total{backend="fake"} 2`,
		`test_rate_limit_waits_total{limiter="memory"} 1`,
		`test_rate_limit_wait_seconds_bucket`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in metrics:\n%s", want, body)
		}
	}
}

func TestRateLimitedBackend_EmitsLLMCompleteSpan(t *testing.T) {
	// Install a real trace provider whose slog span lines land in a buffer,
	// then restore both the default logger and the global provider on cleanup.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "") // no OTLP exporter in tests
	buf := &bytes.Buffer{}
	oldLogger := slog.Default()
	// Tag the logger with the service name, mirroring internal/log.Setup.
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "test"))
	p, err := trace.Setup("test")
	if err != nil {
		t.Fatalf("trace.Setup: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		slog.SetDefault(oldLogger)
	})

	// Success path through the fast path (limiter == nil).
	backend := &dummyLLM{name: "fake", usage: Usage{PromptTokens: 10, CompletionTokens: 5}}
	rl := NewRateLimitedBackend(backend, nil, nil)

	if _, err := rl.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Error path through the rate-limited path.
	clock := ratelimit.NewManualClock(time.Now())
	bucket := ratelimit.NewMemoryBucket(1000, time.Minute, clock)
	errBackend := &dummyLLM{name: "fake", err: errors.New("boom")}
	rlErr := NewRateLimitedBackend(errBackend, bucket, nil)

	if _, err := rlErr.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello again"}},
	}); err == nil {
		t.Fatal("expected error from backend")
	}

	out := buf.String()
	for _, want := range []string{
		`name=llm.complete`,
		`backend=fake`,
		`service=test`,
		`status=ok`,
		`status=error`,
		`error=boom`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected span output to contain %q, got:\n%s", want, out)
		}
	}
}