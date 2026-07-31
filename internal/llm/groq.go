package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type GroqBackend struct {
	host       string
	apiKey     string
	model      string
	client     *http.Client
	maxRetries int
}

func NewGroqBackend(host, apiKey, model string, client *http.Client, maxRetries int) *GroqBackend {
	if client == nil {
		client = http.DefaultClient
	}
	if host == "" {
		host = "https://api.groq.com"
	}
	return &GroqBackend{
		host:       strings.TrimRight(host, "/"),
		apiKey:     apiKey,
		model:      model,
		client:     client,
		maxRetries: maxRetries,
	}
}

func (g *GroqBackend) Name() string {
	return "groq"
}

type groqResponseFormat struct {
	Type string `json:"type"`
}

type groqChatReq struct {
	Model          string              `json:"model"`
	Messages       []Message           `json:"messages"`
	ResponseFormat *groqResponseFormat `json:"response_format,omitempty"`
}

type groqChatChoice struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type groqChatResp struct {
	Choices []groqChatChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (g *GroqBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	return retryTransient(ctx, g.maxRetries, func() (CompleteResponse, error) {
		bodyReq := groqChatReq{
			Model:    g.model,
			Messages: req.Messages,
		}
		if req.JSON {
			bodyReq.ResponseFormat = &groqResponseFormat{Type: "json_object"}
		}

		payload, err := json.Marshal(bodyReq)
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("groq marshal: %w", err)
		}

		url := g.host + "/openai/v1/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("groq req build: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if g.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
		}

		resp, err := g.client.Do(httpReq)
		if err != nil {
			return CompleteResponse{}, err
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return CompleteResponse{}, fmt.Errorf("groq read body: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var retryAfter time.Duration
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					retryAfter = time.Duration(secs) * time.Second
				}
			}
			return CompleteResponse{}, &HTTPError{
				StatusCode: resp.StatusCode,
				Status:     resp.Status,
				Body:       string(respBody),
				RetryAfter: retryAfter,
			}
		}

		var gResp groqChatResp
		if err := json.Unmarshal(respBody, &gResp); err != nil {
			return CompleteResponse{}, fmt.Errorf("groq unmarshal: %w", err)
		}

		if len(gResp.Choices) == 0 {
			return CompleteResponse{}, fmt.Errorf("groq response contained 0 choices")
		}

		firstChoice := gResp.Choices[0]
		return CompleteResponse{
			Content: firstChoice.Message.Content,
			Usage: Usage{
				PromptTokens:     gResp.Usage.PromptTokens,
				CompletionTokens: gResp.Usage.CompletionTokens,
			},
			FinishReason: firstChoice.FinishReason,
		}, nil
	})
}