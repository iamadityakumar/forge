package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`    // system | user | assistant
	Content string `json:"content"`
}

type CompleteRequest struct {
	Messages []Message `json:"messages"`
	JSON     bool      `json:"json"` // force JSON mode
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type CompleteResponse struct {
	Content      string `json:"content"`
	Usage        Usage  `json:"usage"`
	FinishReason string `json:"finish_reason"`
}

type LLMBackend interface {
	Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
	Name() string
}

// HTTPError represents a non-2xx HTTP response from an LLM provider.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("llm http error %d (%s): %s", e.StatusCode, e.Status, e.Body)
}

func isTransientErr(err error) (bool, time.Duration) {
	if err == nil {
		return false, 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, 0
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == 429 || httpErr.StatusCode >= 500 {
			return true, httpErr.RetryAfter
		}
		return false, 0
	}

	// Network / timeout errors are transient
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true, 0
	}

	return false, 0
}

func computeBackoff(attempt int) time.Duration {
	base := time.Second
	cap := 30 * time.Second

	// Exp backoff: base * 2^attempt
	d := base << uint(attempt)
	if d > cap || d <= 0 {
		d = cap
	}

	// Jitter: +0.4 * rand
	jitter := time.Duration(rand.Float64() * 0.4 * float64(d))
	d = d + jitter
	if d > cap {
		d = cap
	}
	return d
}

func retryTransient(ctx context.Context, maxRetries int, do func() (CompleteResponse, error)) (CompleteResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return CompleteResponse{}, err
		}

		resp, err := do()
		if err == nil {
			return resp, nil
		}

		lastErr = err
		transient, retryAfter := isTransientErr(err)
		if !transient || attempt == maxRetries {
			return CompleteResponse{}, err
		}

		delay := computeBackoff(attempt)
		if retryAfter > 0 {
			delay = retryAfter
		}

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return CompleteResponse{}, ctx.Err()
		}
	}
	return CompleteResponse{}, lastErr
}

func NewFromEnv() (LLMBackend, error) {
	backendName := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_BACKEND")))
	if backendName == "" {
		backendName = "ollama"
	}

	timeout := 60 * time.Second
	if tStr := os.Getenv("LLM_TIMEOUT"); tStr != "" {
		if d, err := time.ParseDuration(tStr); err == nil {
			timeout = d
		}
	}

	maxRetries := 3
	if rStr := os.Getenv("LLM_MAX_RETRIES"); rStr != "" {
		if r, err := strconv.Atoi(rStr); err == nil && r >= 0 {
			maxRetries = r
		}
	}

	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}

	switch backendName {
	case "ollama":
		host := os.Getenv("OLLAMA_HOST")
		if host == "" {
			host = "http://localhost:11434"
		}
		model := os.Getenv("OLLAMA_MODEL")
		if model == "" {
			model = "llama3.1"
		}
		return NewOllamaBackend(host, model, client, maxRetries), nil

	case "groq":
		apiKey := os.Getenv("GROQ_API_KEY")
		if apiKey == "" {
			return nil, errors.New("GROQ_API_KEY is required when LLM_BACKEND=groq")
		}
		baseURL := os.Getenv("GROQ_BASE_URL")
		if baseURL == "" {
			baseURL = "https://api.groq.com/openai/v1"
		}
		model := os.Getenv("GROQ_MODEL")
		if model == "" {
			model = "llama-3.1-8b-instant"
		}
		return NewGroqBackend(baseURL, apiKey, model, client, maxRetries), nil

	case "fake":
		return NewFakeBackend(), nil

	default:
		return nil, fmt.Errorf("unknown LLM_BACKEND %q (expected ollama|groq|fake)", backendName)
	}
}