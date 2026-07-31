package llm

import (
	"context"
	"sync"
	"sync/atomic"
)

type FakeBackend struct {
	calls        int32
	mu           sync.Mutex
	responses    []CompleteResponse
	responseFunc func(req CompleteRequest) (CompleteResponse, error)
}

func NewFakeBackend(responses ...CompleteResponse) *FakeBackend {
	return &FakeBackend{
		responses: responses,
	}
}

func NewFakeBackendWithFunc(fn func(req CompleteRequest) (CompleteResponse, error)) *FakeBackend {
	return &FakeBackend{
		responseFunc: fn,
	}
}

func (f *FakeBackend) Name() string {
	return "fake"
}

func (f *FakeBackend) CallCount() int {
	return int(atomic.LoadInt32(&f.calls))
}

func (f *FakeBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	callIdx := atomic.AddInt32(&f.calls, 1) - 1

	if f.responseFunc != nil {
		return f.responseFunc(req)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.responses) == 0 {
		return CompleteResponse{
			Content:      `{"action": "finish", "answer": "fake default response"}`,
			Usage:        Usage{PromptTokens: 10, CompletionTokens: 5},
			FinishReason: "stop",
		}, nil
	}

	if int(callIdx) < len(f.responses) {
		return f.responses[callIdx], nil
	}

	return f.responses[len(f.responses)-1], nil
}

func (f *FakeBackend) SetResponses(responses ...CompleteResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = responses
}