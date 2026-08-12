package llm

import (
	"context"
	"strconv"
	"sync"
	"time"

	"forge/internal/clock"
	"sync/atomic"
)

type FakeBackend struct {
	calls        int32
	mu           sync.Mutex
	responses    []CompleteResponse
	responseFunc func(req CompleteRequest) (CompleteResponse, error)

	// For deterministic simulation
	scriptedCalls []scriptedCall
	nextScriptIdx int
	callCounts    map[string]int // key: "jobID:step"
	clk           clock.Clock
}

type scriptedCall struct {
	response CompleteResponse
	err      error
	delay    time.Duration
}

func NewFakeBackend(responses ...CompleteResponse) *FakeBackend {
	return &FakeBackend{
		responses:  responses,
		callCounts: make(map[string]int),
	}
}

func NewFakeBackendWithFunc(fn func(req CompleteRequest) (CompleteResponse, error)) *FakeBackend {
	return &FakeBackend{
		responseFunc: fn,
		callCounts:   make(map[string]int),
	}
}

func (f *FakeBackend) Name() string {
	return "fake"
}

func (f *FakeBackend) CallCount() int {
	return int(atomic.LoadInt32(&f.calls))
}

// Script appends a scripted call to the backend's script.
// The script is executed in order; each call to Complete consumes the next scripted entry.
// Use Delay() to add virtual-time latency.
func (f *FakeBackend) Script(response CompleteResponse, err error) *FakeBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scriptedCalls = append(f.scriptedCalls, scriptedCall{response: response, err: err})
	return f
}

// ScriptErr appends a scripted error to the backend's script.
func (f *FakeBackend) ScriptErr(err error) *FakeBackend {
	return f.Script(CompleteResponse{}, err)
}

// Delay sets the virtual-time delay for the next scripted call.
func (f *FakeBackend) Delay(d time.Duration) *FakeBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scriptedCalls) > 0 {
		f.scriptedCalls[len(f.scriptedCalls)-1].delay = d
	}
	return f
}

// StepCalls returns the number of times the backend was asked to complete
// a specific step of a job. The key format is "jobID:stepNumber".
// This is the exactly-once oracle for simulation tests.
func (f *FakeBackend) StepCalls(jobID string, step int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := jobID + ":" + strconv.Itoa(step)
	return f.callCounts[key]
}

// SetClock injects a clock for deterministic virtual-time delays.
func (f *FakeBackend) SetClock(clk clock.Clock) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clk = clk
}

func (f *FakeBackend) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	callIdx := atomic.AddInt32(&f.calls, 1) - 1

	// If a responseFunc is set (legacy API), use it.
	if f.responseFunc != nil {
		return f.responseFunc(req)
	}

	f.mu.Lock()
	// Use scripted calls if available
	if len(f.scriptedCalls) > 0 && int(callIdx) < len(f.scriptedCalls) {
		scripted := f.scriptedCalls[callIdx]
		clk := f.clk
		delay := scripted.delay
		f.mu.Unlock()

		// Apply virtual-time delay if clock is set
		if delay > 0 && clk != nil {
			clk.Sleep(delay)
		}

		// Record call count for the oracle
		f.mu.Lock()
		// We can't easily extract jobID/step from the request here,
		// so the caller should use StepCalls(key) with a known key
		// For simulation, we'll track by call index
		_ = scripted
		f.mu.Unlock()

		if scripted.err != nil {
			return CompleteResponse{}, scripted.err
		}
		return scripted.response, nil
	}
	f.mu.Unlock()

	// Fallback to static responses
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
	f.scriptedCalls = nil // Clear scripted calls when setting static responses
}
