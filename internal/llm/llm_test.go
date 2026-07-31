package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestOllamaBackend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("expected path /api/chat, got %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"message": {"role": "assistant", "content": "{\"status\":\"ok\"}"},
			"done": true,
			"done_reason": "stop",
			"prompt_eval_count": 15,
			"eval_count": 25
		}`))
	}))
	defer ts.Close()

	backend := NewOllamaBackend(ts.URL, "llama3", ts.Client(), 2)
	if backend.Name() != "ollama" {
		t.Fatalf("expected name ollama, got %s", backend.Name())
	}

	resp, err := backend.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		JSON:     true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Content != `{"status":"ok"}` {
		t.Errorf("unexpected content: %s", resp.Content)
	}
	if resp.Usage.PromptTokens != 15 || resp.Usage.CompletionTokens != 25 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

func TestGroqBackend_RetryAfterHeader(t *testing.T) {
	var attempts int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Authorization header Bearer test-key")
		}

		if count == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limit"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"choices": [{
				"message": {"role": "assistant", "content": "hello world"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5}
		}`))
	}))
	defer ts.Close()

	backend := NewGroqBackend(ts.URL, "test-key", "llama-3.3-70b", ts.Client(), 2)
	resp, err := backend.Complete(context.Background(), CompleteRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("expected success on second attempt, got: %v", err)
	}

	if resp.Content != "hello world" {
		t.Errorf("expected 'hello world', got: %s", resp.Content)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected 2 attempts, got: %d", attempts)
	}
}

func TestRetryTransient_NonTransientErrorAborts(t *testing.T) {
	var attempts int32
	ctx := context.Background()

	_, err := retryTransient(ctx, 3, func() (CompleteResponse, error) {
		atomic.AddInt32(&attempts, 1)
		return CompleteResponse{}, &HTTPError{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Body:       "invalid payload",
		}
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("expected 1 attempt for 400 error, got %d", attempts)
	}
}

func TestRetryTransient_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := retryTransient(ctx, 3, func() (CompleteResponse, error) {
		return CompleteResponse{}, &HTTPError{StatusCode: 500}
	})

	if err == nil {
		t.Fatal("expected error on canceled context")
	}
}

func TestFakeBackend(t *testing.T) {
	fb := NewFakeBackend(
		CompleteResponse{Content: "resp 1"},
		CompleteResponse{Content: "resp 2"},
	)

	if fb.Name() != "fake" {
		t.Errorf("expected name 'fake', got %s", fb.Name())
	}

	r1, err := fb.Complete(context.Background(), CompleteRequest{})
	if err != nil || r1.Content != "resp 1" {
		t.Fatalf("expected resp 1, got %v, err: %v", r1, err)
	}

	r2, err := fb.Complete(context.Background(), CompleteRequest{})
	if err != nil || r2.Content != "resp 2" {
		t.Fatalf("expected resp 2, got %v, err: %v", r2, err)
	}

	if fb.CallCount() != 2 {
		t.Errorf("expected 2 calls, got %d", fb.CallCount())
	}
}

func TestNewFromEnv(t *testing.T) {
	os.Setenv("LLM_BACKEND", "fake")
	b1, err := NewFromEnv()
	if err != nil || b1.Name() != "fake" {
		t.Errorf("expected fake backend, got %v, err: %v", b1, err)
	}

	os.Setenv("LLM_BACKEND", "ollama")
	os.Setenv("OLLAMA_HOST", "http://localhost:11434")
	os.Setenv("OLLAMA_MODEL", "qwen2.5-coder")
	b2, err := NewFromEnv()
	if err != nil || b2.Name() != "ollama" {
		t.Errorf("expected ollama backend, got %v, err: %v", b2, err)
	}

	os.Setenv("LLM_BACKEND", "invalid_backend")
	_, err = NewFromEnv()
	if err == nil {
		t.Errorf("expected error for invalid backend")
	}

	os.Unsetenv("LLM_BACKEND")
}