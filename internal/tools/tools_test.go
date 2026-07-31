package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	kbTool := NewSearchKBTool()

	if err := r.Register(kbTool); err != nil {
		t.Fatalf("failed to register tool: %v", err)
	}

	if err := r.Register(kbTool); err == nil {
		t.Fatal("expected error on duplicate tool registration")
	}

	tool, ok := r.Get("search_kb")
	if !ok || tool.Name() != "search_kb" {
		t.Fatalf("failed to get tool search_kb")
	}

	if len(r.List()) != 1 {
		t.Fatalf("expected 1 registered tool, got %d", len(r.List()))
	}
}

func TestSearchKBTool(t *testing.T) {
	kb := NewSearchKBTool()

	res, err := kb.Execute(context.Background(), `{"query": "prefix"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(strings.ToLower(res), "prefix") {
		t.Errorf("expected search result to contain 'prefix', got: %s", res)
	}

	noMatch, err := kb.Execute(context.Background(), `{"query": "nonexistent_term_xyz"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(noMatch, "No knowledge base documents found") {
		t.Errorf("expected no documents found message, got: %s", noMatch)
	}
}

func TestRunTestsTool_PassingAndFailing(t *testing.T) {
	runner := NewRunTestsTool()

	code := `
import sys
input_data = sys.stdin.read().split()
if input_data:
    a, b = map(int, input_data[:2])
    print(a + b)
`
	args := RunTestsArgs{
		Code: code,
		TestCases: []TestCase{
			{Input: "2 3\n", Output: "5"},
			{Input: "10 20\n", Output: "30"},
			{Input: "1 1\n", Output: "3"}, // intentionally failing
		},
	}

	argsBytes, _ := json.Marshal(args)
	resStr, err := runner.Execute(context.Background(), string(argsBytes))
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	var output RunTestsOutput
	if err := json.Unmarshal([]byte(resStr), &output); err != nil {
		t.Fatalf("failed to parse run_tests output json: %v", err)
	}

	if output.TotalCases != 3 {
		t.Errorf("expected 3 total cases, got %d", output.TotalCases)
	}
	if output.PassedCases != 2 {
		t.Errorf("expected 2 passed cases, got %d", output.PassedCases)
	}
	if output.Success {
		t.Errorf("expected success to be false due to 1 failed case")
	}
	if output.Results[2].Passed {
		t.Errorf("expected 3rd test case to fail")
	}
}

func TestRunTestsTool_Timeout(t *testing.T) {
	runner := NewRunTestsTool()

	code := `
import time
time.sleep(5)
`
	args := RunTestsArgs{
		Code: code,
		TestCases: []TestCase{
			{Input: "", Output: ""},
		},
		TimeoutMs: 200,
	}

	argsBytes, _ := json.Marshal(args)
	start := time.Now()
	resStr, err := runner.Execute(context.Background(), string(argsBytes))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	if elapsed > 2*time.Second {
		t.Errorf("execution took too long: %v", elapsed)
	}

	var output RunTestsOutput
	if err := json.Unmarshal([]byte(resStr), &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if output.Results[0].Passed {
		t.Errorf("expected test case to fail due to timeout")
	}
	if !strings.Contains(output.Results[0].Error, "time limit exceeded") {
		t.Errorf("expected 'time limit exceeded' error, got: %s", output.Results[0].Error)
	}
}