package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"forge/internal/store"
)

func TestGraphAgent_Run_Success(t *testing.T) {
	// Mock sidecar returns complete graph execution
	sidecarEvents := []graphEvent{
		{Event: "node_start", Node: "plan", Step: 1},
		{Event: "node_done", Node: "plan", Step: 1, Data: json.RawMessage(`{"plan":"initial plan"}`)},
		{Event: "node_start", Node: "kb_search", Step: 2},
		{Event: "node_done", Node: "kb_search", Step: 2, Data: json.RawMessage(`{"last_observation":"kb result"}`)},
		{Event: "node_start", Node: "write_solution", Step: 3},
		{Event: "node_done", Node: "write_solution", Step: 3, Data: json.RawMessage(`{"solution":"def two_sum():\\n    pass"}`)},
		{Event: "node_start", Node: "run_tests", Step: 4},
		{Event: "node_done", Node: "run_tests", Step: 4, Data: json.RawMessage(`{"tests_passed":true,"attempts":1}`)},
		{Event: "node_start", Node: "verify", Step: 5},
		{Event: "node_done", Node: "verify", Step: 5, Data: json.RawMessage(`{"done":true,"tests_passed":true}`)},
		{Event: "done", TestsPassed: true, Attempts: 1, Solution: "correct", Done: true},
	}

	var ndjson bytes.Buffer
	for _, e := range sidecarEvents {
		line, _ := json.Marshal(e)
		ndjson.Write(line)
		ndjson.WriteString("\n")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path == "/run" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.Write(ndjson.Bytes())
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	ms := newMemoryStore()
	jobVal, err := ms.CreateJob(context.Background(), "graph_solve", json.RawMessage(`{"prompt":"Two Sum"}`), 0, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	job := &jobVal
	job.LeaseEpoch = 1

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
		MaxSteps:   24,
	})

	err = agent.Run(context.Background(), ms, job, 1, "test-worker-1")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	steps, _ := ms.ListSteps(context.Background(), job.ID)
	if len(steps) != 5 {
		t.Errorf("Expected 5 steps (one per node_done), got %d", len(steps))
	}

	for _, step := range steps {
		if step.StepType != StepTypeGraphNode {
			t.Errorf("Expected step type %q, got %q", StepTypeGraphNode, step.StepType)
		}
		if step.WorkerID != "test-worker-1" {
			t.Errorf("Expected worker ID test-worker-1, got %q", step.WorkerID)
		}
	}
}

func TestGraphAgent_Run_RetryThenPass(t *testing.T) {
	// First attempt fails, second passes
	sidecarEvents := []graphEvent{
		{Event: "node_done", Node: "plan", Step: 1, Data: json.RawMessage(`{"plan":"try again"}`)},
		{Event: "node_done", Node: "kb_search", Step: 2, Data: json.RawMessage(`{"last_observation":""}`)},
		{Event: "node_done", Node: "write_solution", Step: 3, Data: json.RawMessage(`{"solution":"bad"}`)},
		{Event: "node_done", Node: "run_tests", Step: 4, Data: json.RawMessage(`{"tests_passed":false,"attempts":1}`)},
		{Event: "node_done", Node: "verify", Step: 5, Data: json.RawMessage(`{"done":false}`)},
		// Retry: write_solution again
		{Event: "node_done", Node: "write_solution", Step: 6, Data: json.RawMessage(`{"solution":"good"}`)},
		{Event: "node_done", Node: "run_tests", Step: 7, Data: json.RawMessage(`{"tests_passed":true,"attempts":2}`)},
		{Event: "node_done", Node: "verify", Step: 8, Data: json.RawMessage(`{"done":true,"tests_passed":true}`)},
		{Event: "done", TestsPassed: true, Attempts: 2, Done: true},
	}

	var ndjson bytes.Buffer
	for _, e := range sidecarEvents {
		line, _ := json.Marshal(e)
		ndjson.Write(line)
		ndjson.WriteString("\n")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(ndjson.Bytes())
	}))
	defer server.Close()

	ms := newMemoryStore()
	jobVal, _ := ms.CreateJob(context.Background(), "graph_solve", json.RawMessage(`{"prompt":"Two Sum"}`), 0, "")
	job := &jobVal
	job.LeaseEpoch = 1

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
		MaxSteps:   24,
	})

	err := agent.Run(context.Background(), ms, job, 1, "worker-1")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	steps, _ := ms.ListSteps(context.Background(), job.ID)
	if len(steps) != 8 {
		t.Errorf("Expected 8 steps (8 node_done events), got %d", len(steps))
	}
}

func TestGraphAgent_Run_Fenced(t *testing.T) {
	// Simulate fenced error mid-stream
	sidecarEvents := []graphEvent{
		{Event: "node_done", Node: "plan", Step: 1, Data: json.RawMessage(`{}`)},
		{Event: "node_done", Node: "kb_search", Step: 2, Data: json.RawMessage(`{}`)},
		{Event: "done", TestsPassed: true, Attempts: 1, Done: true},
	}

	var ndjson bytes.Buffer
	for _, e := range sidecarEvents {
		line, _ := json.Marshal(e)
		ndjson.Write(line)
		ndjson.WriteString("\n")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(ndjson.Bytes())
	}))
	defer server.Close()

	ms := newMemoryStore()
	jobVal, _ := ms.CreateJob(context.Background(), "graph_solve", json.RawMessage(`{"prompt":"test"}`), 0, "")
	job := &jobVal
	job.LeaseEpoch = 1

	// Wrap to fence on step 2
	fencingStore := &fenceOnStepStore{
		memoryStore: ms,
		targetStep:  2,
	}

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
		MaxSteps:   24,
	})

	err := agent.Run(context.Background(), fencingStore, job, 1, "worker-1")
	if err == nil {
		t.Fatal("Expected fenced error, got nil")
	}
	if !strings.Contains(err.Error(), "graph agent") && !strings.Contains(err.Error(), store.ErrFenced.Error()) {
		t.Errorf("Expected fenced-related error, got: %v", err)
	}
}

func TestGraphAgent_Run_SidecarError(t *testing.T) {
	sidecarEvents := []graphEvent{
		{Event: "error", Message: "GROQ_API_KEY not set"},
	}

	var ndjson bytes.Buffer
	for _, e := range sidecarEvents {
		line, _ := json.Marshal(e)
		ndjson.Write(line)
		ndjson.WriteString("\n")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(ndjson.Bytes())
	}))
	defer server.Close()

	ms := newMemoryStore()
	jobVal, _ := ms.CreateJob(context.Background(), "graph_solve", json.RawMessage(`{"prompt":"test"}`), 0, "")
	job := &jobVal
	job.LeaseEpoch = 1

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
		MaxSteps:   24,
	})

	err := agent.Run(context.Background(), ms, job, 1, "worker-1")
	if err == nil {
		t.Fatal("Expected graph error, got nil")
	}
	if !strings.Contains(err.Error(), "graph error") {
		t.Errorf("Expected graph error message, got: %v", err)
	}
}

func TestGraphAgent_Run_ExhaustedWithoutPass(t *testing.T) {
	// Graph finishes but didn't pass tests
	sidecarEvents := []graphEvent{
		{Event: "node_done", Node: "plan", Step: 1, Data: json.RawMessage(`{}`)},
		{Event: "node_done", Node: "run_tests", Step: 2, Data: json.RawMessage(`{"tests_passed":false,"attempts":3}`)},
		{Event: "done", TestsPassed: false, Attempts: 3, Done: true},
	}

	var ndjson bytes.Buffer
	for _, e := range sidecarEvents {
		line, _ := json.Marshal(e)
		ndjson.Write(line)
		ndjson.WriteString("\n")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(ndjson.Bytes())
	}))
	defer server.Close()

	ms := newMemoryStore()
	jobVal, _ := ms.CreateJob(context.Background(), "graph_solve", json.RawMessage(`{"prompt":"test"}`), 0, "")
	job := &jobVal
	job.LeaseEpoch = 1

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
		MaxSteps:   24,
	})

	err := agent.Run(context.Background(), ms, job, 1, "worker-1")
	if err == nil {
		t.Fatal("Expected error for failed graph, got nil")
	}
	if !strings.Contains(err.Error(), "without passing tests") {
		t.Errorf("Expected 'without passing tests' error, got: %v", err)
	}
}

func TestGraphAgent_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer server.Close()

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
	})

	err := agent.HealthCheck(context.Background())
	if err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestGraphAgent_HealthCheck_Unavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	agent := NewGraphAgent(GraphAgentConfig{
		SidecarURL: server.URL,
	})

	err := agent.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("Expected health check error, got nil")
	}
}

func TestParseGraphTask(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "prompt field",
			payload: `{"prompt":"Two Sum problem"}`,
			want:    "Two Sum problem",
		},
		{
			name:    "raw string",
			payload: `Two Sum problem`,
			want:    "Two Sum problem",
		},
		{
			name:    "empty prompt fallback",
			payload: `{"prompt":""}`,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseGraphTask(json.RawMessage(tt.payload))
			if result != tt.want {
				t.Errorf("parseGraphTask() = %q, want %q", result, tt.want)
			}
		})
	}
}

// fenceOnStepStore wraps memoryStore and returns ErrFenced on a target step.
type fenceOnStepStore struct {
	*memoryStore
	targetStep int
}

func (f *fenceOnStepStore) RecordStep(ctx context.Context, id uuid.UUID, epoch int, step store.JobStep) (uuid.UUID, error) {
	if step.StepNumber == f.targetStep {
		return uuid.Nil, store.ErrFenced
	}
	return f.memoryStore.RecordStep(ctx, id, epoch, step)
}