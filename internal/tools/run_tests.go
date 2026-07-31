package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type TestCase struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type RunTestsArgs struct {
	Code      string     `json:"code"`
	TestCases []TestCase `json:"test_cases"`
	TimeoutMs int        `json:"timeout_ms,omitempty"`
}

type RunTestsTool struct{}

func NewRunTestsTool() *RunTestsTool {
	return &RunTestsTool{}
}

func (r *RunTestsTool) Name() string {
	return "run_tests"
}

func (r *RunTestsTool) Description() string {
	return "Executes Python solution code against a list of test cases in a sandboxed temporary environment."
}

func (r *RunTestsTool) Schema() string {
	return `{"type":"object","properties":{"code":{"type":"string","description":"Python solution code to run"},"test_cases":{"type":"array","items":{"type":"object","properties":{"input":{"type":"string"},"output":{"type":"string"}},"required":["input","output"]}},"timeout_ms":{"type":"integer","description":"Optional per-test timeout in milliseconds (default 2000ms)"}},"required":["code","test_cases"]}`
}

type TestResult struct {
	CaseIndex int    `json:"case_index"`
	Passed    bool   `json:"passed"`
	Got       string `json:"got,omitempty"`
	Expected  string `json:"expected"`
	Error     string `json:"error,omitempty"`
}

type RunTestsOutput struct {
	Success     bool         `json:"success"`
	TotalCases  int          `json:"total_cases"`
	PassedCases int          `json:"passed_cases"`
	Results     []TestResult `json:"results"`
}

func (r *RunTestsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args RunTestsArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if strings.TrimSpace(args.Code) == "" {
		return "", fmt.Errorf("code cannot be empty")
	}
	if len(args.TestCases) == 0 {
		return "", fmt.Errorf("test_cases cannot be empty")
	}

	timeout := parseTimeoutEnv()
	if args.TimeoutMs > 0 {
		timeout = time.Duration(args.TimeoutMs) * time.Millisecond
	}

	tmpDir, err := os.MkdirTemp("", "forge-runner-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	scriptPath := filepath.Join(tmpDir, "solution.py")
	if err := os.WriteFile(scriptPath, []byte(args.Code), 0600); err != nil {
		return "", fmt.Errorf("failed to write solution file: %w", err)
	}

	output := RunTestsOutput{
		TotalCases: len(args.TestCases),
		Results:    make([]TestResult, 0, len(args.TestCases)),
	}

	for i, tc := range args.TestCases {
		testCtx, testCancel := context.WithTimeout(ctx, timeout)
		got, execErr := executePythonScript(testCtx, tmpDir, scriptPath, tc.Input)
		testCancel()

		tr := TestResult{
			CaseIndex: i + 1,
			Expected:  strings.TrimSpace(tc.Output),
		}

		if execErr != nil {
			tr.Passed = false
			tr.Error = execErr.Error()
		} else {
			gotClean := strings.TrimSpace(got)
			tr.Got = gotClean
			if gotClean == tr.Expected {
				tr.Passed = true
				output.PassedCases++
			} else {
				tr.Passed = false
			}
		}
		output.Results = append(output.Results, tr)
	}

	output.Success = (output.PassedCases == output.TotalCases)
	outBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal output: %w", err)
	}

	return string(outBytes), nil
}

func parseTimeoutEnv() time.Duration {
	if val := os.Getenv("RUN_TESTS_TIMEOUT_MS"); val != "" {
		if ms, err := strconv.Atoi(val); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 2 * time.Second
}