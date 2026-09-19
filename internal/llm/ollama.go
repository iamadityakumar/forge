package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"forge/internal/trace"
)

type OllamaBackend struct {
	host       string
	model      string
	client     *http.Client
	maxRetries int
}

type OllamaEmbeddingBackend struct {
	host   string
	model  string
	client *http.Client
}

func NewOllamaEmbeddingBackend(host, model string, client *http.Client) *OllamaEmbeddingBackend {
	if client == nil {
		client = http.DefaultClient
	}
	return &OllamaEmbeddingBackend{host: strings.TrimRight(host, "/"), model: model, client: client}
}

func (o *OllamaEmbeddingBackend) Name() string { return "ollama-embedding" }
func (o *OllamaEmbeddingBackend) Dimensions() int {
	if d := os.Getenv("EMBEDDING_DIMENSIONS"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			return n
		}
	}
	return 768
}

func (o *OllamaEmbeddingBackend) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{o.model, text})
	if err != nil {
		return nil, fmt.Errorf("ollama embedding marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama embedding read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(respBody)}
	}
	var result struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("ollama embedding decode: %w", err)
	}
	if len(result.Embeddings) != 1 || len(result.Embeddings[0]) == 0 {
		return nil, errors.New("ollama embedding response contained no vector")
	}
	return result.Embeddings[0], nil
}

func NewOllamaBackend(host, model string, client *http.Client, maxRetries int) *OllamaBackend {
	if client == nil {
		client = http.DefaultClient
	}
	return &OllamaBackend{
		host:       strings.TrimRight(host, "/"),
		model:      model,
		client:     client,
		maxRetries: maxRetries,
	}
}

func (o *OllamaBackend) Name() string {
	return "ollama"
}

type ollamaChatReq struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Format   string    `json:"format,omitempty"`
}

type ollamaChatResp struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

func (o *OllamaBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	return retryTransient(ctx, o.maxRetries, func() (CompleteResponse, error) {
		bodyReq := ollamaChatReq{
			Model:    o.model,
			Messages: req.Messages,
			Stream:   false,
		}
		if req.JSON {
			bodyReq.Format = "json"
		}

		payload, err := json.Marshal(bodyReq)
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("ollama marshal: %w", err)
		}

		url := o.host + "/api/chat"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("ollama req build: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		trace.InjectW3C(ctx, httpReq)

		resp, err := o.client.Do(httpReq)
		if err != nil {
			return CompleteResponse{}, err
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("ollama read body: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return CompleteResponse{}, &HTTPError{
				StatusCode: resp.StatusCode,
				Status:     resp.Status,
				Body:       string(respBody),
			}
		}

		var oResp ollamaChatResp
		if err := json.Unmarshal(respBody, &oResp); err != nil {
			return CompleteResponse{}, fmt.Errorf("ollama unmarshal: %w", err)
		}

		return CompleteResponse{
			Content:      oResp.Message.Content,
			Usage:        Usage{PromptTokens: oResp.PromptEvalCount, CompletionTokens: oResp.EvalCount},
			FinishReason: oResp.DoneReason,
		}, nil
	})
}
