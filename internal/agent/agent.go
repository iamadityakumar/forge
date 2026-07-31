package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"forge/internal/llm"
	"forge/internal/store"
	"forge/internal/tools"
)

// Step types recorded in job_steps. Week 4 replaces the Week-3 "segment"
// step type with a two-row per-iteration protocol:
//   step 2k-1 → "plan"     (the LLM decision; committed BEFORE tool execution)
//   step 2k   → "tool_call" (the observation / tool result)
const (
	StepTypePlan     = "plan"
	StepTypeToolCall = "tool_call"
)

// Agent implements worker.Handler for the cp_solve task_type. It runs a
// dynamic Plan -> Act -> Observe loop where every LLM decision (plan) and every
// tool execution (tool_call) is a resumable, fenced step in PostgreSQL.
//
// Crash-recovery contract: a plan row is committed before its tool runs, so if
// the worker is kill -9'd between the two, a reclaiming worker reconstructs the
// history, detects the lone trailing plan, and re-executes the tool WITHOUT
// re-calling the LLM API (zero re-spend on resume).
type Agent struct {
	backend    llm.LLMBackend
	registry   *tools.Registry
	maxSteps   int
	systemProm string
}

func New(backend llm.LLMBackend, registry *tools.Registry) *Agent {
	maxSteps := 10
	if envVal := os.Getenv("AGENT_MAX_STEPS"); envVal != "" {
		if ms, err := strconv.Atoi(envVal); err == nil && ms > 0 {
			maxSteps = ms
		}
	}

	return &Agent{
		backend:    backend,
		registry:   registry,
		maxSteps:   maxSteps,
		systemProm: buildSystemPrompt(registry),
	}
}

func (a *Agent) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
	steps, err := s.ListSteps(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("agent list steps: %w", err)
	}

	// Rebuild the conversation from committed rows. If the last committed row is
	// a lone plan (uncommitted tool execution from a crashed worker), pendingDecision
	// is set and we re-execute the tool without re-calling the LLM.
	messages, pendingDecision, completedIterations, err := reconstructHistory(steps, a.systemProm, jobPrompt(job.Payload))
	if err != nil {
		return fmt.Errorf("agent reconstruct history: %w", err)
	}

	currentStepNum := len(steps)
	iteration := completedIterations

	// The committed history can already end in a finish decision if the previous
	// worker crashed after recording its final plan but before the worker-loop
	// CompleteJob. The finish is already durable — just return nil and let the
	// worker loop transition the job to completed. No LLM call, no tool run.
	if pendingDecision != nil && pendingDecision.Action == "finish" {
		return nil
	}

	if pendingDecision != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		toolOutput := executeToolCall(ctx, a.registry, pendingDecision)
		currentStepNum++
		if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: currentStepNum,
			StepType:   StepTypeToolCall,
			Output:     toolOutputRaw(toolOutput),
			WorkerID:   workerID,
		}); err != nil {
			return fmt.Errorf("record pending tool_call step %d: %w", currentStepNum, err)
		}
		messages = append(messages, llm.Message{
			Role:    "user",
			Content: fmt.Sprintf("Tool Output (%s):\n%s", pendingDecision.ToolName, toolOutput),
		})
	}

	for iteration < a.maxSteps {
		iteration++

		resp, err := a.backend.Complete(ctx, llm.CompleteRequest{
			Messages: messages,
			JSON:     true,
		})
		if err != nil {
			return fmt.Errorf("llm complete error at iteration %d: %w", iteration, err)
		}

		decision, parseErr := parseDecision(resp.Content)
		if parseErr != nil {
			// Single-nudge recovery: tell the model its output wasn't schema JSON
			// and give it exactly one chance to re-emit.
			nudge := append(messages,
				llm.Message{Role: "assistant", Content: resp.Content},
				llm.Message{Role: "user", Content: `Your response was not valid JSON matching the required schema. Return ONLY a JSON object with "thought" and "action" fields, where "action" is "tool" or "finish".`},
			)
			nudgeResp, retryErr := a.backend.Complete(ctx, llm.CompleteRequest{
				Messages: nudge,
				JSON:     true,
			})
			if retryErr != nil {
				return fmt.Errorf("llm nudge retry error: %w", retryErr)
			}
			decision, parseErr = parseDecision(nudgeResp.Content)
			if parseErr != nil {
				return fmt.Errorf("invalid decision JSON after nudge retry: %w", parseErr)
			}
			resp.Content = nudgeResp.Content
		}

		currentStepNum++
		if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: currentStepNum,
			StepType:   StepTypePlan,
			Output:     json.RawMessage(resp.Content),
			WorkerID:   workerID,
		}); err != nil {
			return fmt.Errorf("record plan step %d: %w", currentStepNum, err)
		}

		messages = append(messages, llm.Message{Role: "assistant", Content: resp.Content})

		if decision.Action == "finish" {
			// The finish decision is itself a committed plan row, so a crash
			// between this and the worker-loop CompleteJob resumes cleanly. A nil
			// return tells the worker loop to transition running -> completed.
			return nil
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		toolOutput := executeToolCall(ctx, a.registry, decision)
		currentStepNum++
		if _, err := s.RecordStep(ctx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: currentStepNum,
			StepType:   StepTypeToolCall,
			Output:     toolOutputRaw(toolOutput),
			WorkerID:   workerID,
		}); err != nil {
			return fmt.Errorf("record tool_call step %d: %w", currentStepNum, err)
		}

		messages = append(messages, llm.Message{
			Role:    "user",
			Content: fmt.Sprintf("Tool Output (%s):\n%s", decision.ToolName, toolOutput),
		})
	}

	return fmt.Errorf("agent exceeded max steps limit of %d", a.maxSteps)
}

// MaxSteps reports the configured iteration cap (used for startup logging).
func (a *Agent) MaxSteps() int { return a.maxSteps }

// jobPrompt extracts the natural-language prompt from the cp_solve payload.
// Accepts either {"prompt": "..."} or a bare-string payload.
func jobPrompt(payload json.RawMessage) string {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &p); err == nil && p.Prompt != "" {
		return p.Prompt
	}
	return string(payload)
}