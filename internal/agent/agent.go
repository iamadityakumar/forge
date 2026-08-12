package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/google/uuid"

	"forge/internal/clock"
	"forge/internal/llm"
	"forge/internal/metrics"
	"forge/internal/store"
	"forge/internal/tools"
	"forge/internal/trace"
)

const (
	StepTypePlan     = "plan"
	StepTypeToolCall = "tool_call"
)

type Agent struct {
	backend    llm.LLMBackend
	registry   *tools.Registry
	maxSteps   int
	systemProm string
	metrics    *metrics.Metrics
	clk        clock.Clock
}

func New(backend llm.LLMBackend, registry *tools.Registry, m *metrics.Metrics) *Agent {
	return NewWithClock(backend, registry, m, clock.SystemClock{})
}

func NewWithClock(backend llm.LLMBackend, registry *tools.Registry, m *metrics.Metrics, clk clock.Clock) *Agent {
	maxSteps := 10
	if envVal := os.Getenv("AGENT_MAX_STEPS"); envVal != "" {
		if ms, err := strconv.Atoi(envVal); err == nil && ms > 0 {
			maxSteps = ms
		}
	}
	if clk == nil {
		clk = clock.SystemClock{}
	}

	return &Agent{
		backend:    backend,
		registry:   registry,
		maxSteps:   maxSteps,
		systemProm: buildSystemPrompt(registry),
		metrics:    m,
		clk:        clk,
	}
}

func (a *Agent) completeAndRecord(ctx context.Context, s store.JobStore, jobID uuid.UUID, workerID string, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	est := llm.EstimateTokens(req)
	t0 := a.clk.Now()
	resp, err := a.backend.Complete(ctx, req)
	latency := int(a.clk.Now().Sub(t0).Milliseconds())

	call := store.LLMCall{
		JobID:           jobID,
		WorkerID:        &workerID,
		Backend:         a.backend.Name(),
		EstimatedTokens: est,
		LatencyMs:       latency,
	}

	if err != nil {
		errStr := err.Error()
		call.Error = &errStr
		_, _ = s.RecordLLMCall(ctx, call)
		return resp, err
	}

	call.PromptTokens = resp.Usage.PromptTokens
	call.CompletionTokens = resp.Usage.CompletionTokens
	_, _ = s.RecordLLMCall(ctx, call)

	return resp, nil
}

func (a *Agent) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
	tracer := trace.NewTracer("agent")

	steps, err := s.ListSteps(ctx, job.ID)
	if err != nil {
		return fmt.Errorf("agent list steps: %w", err)
	}

	if len(steps) > 0 && a.metrics != nil {
		a.metrics.StepsResumed.Add(float64(len(steps)))
	}

	messages, pendingDecision, completedIterations, err := reconstructHistory(steps, a.systemProm, jobPrompt(job.Payload))
	if err != nil {
		return fmt.Errorf("agent reconstruct history: %w", err)
	}

	currentStepNum := len(steps)
	iteration := completedIterations

	if pendingDecision != nil && pendingDecision.Action == "finish" {
		return nil
	}

	if pendingDecision != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		stepCtx, toolSpan := tracer.StartSpan(ctx, "step.tool_call",
			trace.Attribute{Key: "job_id", Value: job.ID.String()},
			trace.Attribute{Key: "step_number", Value: currentStepNum + 1},
			trace.Attribute{Key: "worker_id", Value: workerID},
		)
		toolStart := a.clk.Now()
		toolOutput := executeToolCall(stepCtx, a.registry, pendingDecision)
		currentStepNum++
		if _, err := s.RecordStep(stepCtx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: currentStepNum,
			StepType:   StepTypeToolCall,
			Output:     toolOutputRaw(toolOutput),
			WorkerID:   workerID,
		}); err != nil {
			toolSpan.SetStatus("error", err)
			toolSpan.End()
			return fmt.Errorf("record pending tool_call step %d: %w", currentStepNum, err)
		}
		toolDur := a.clk.Now().Sub(toolStart)
		toolSpan.End()

		if a.metrics != nil {
			a.metrics.StepsTotal.WithLabelValues(StepTypeToolCall).Inc()
			a.metrics.StepDuration.WithLabelValues(StepTypeToolCall).Observe(toolDur.Seconds())
		}

		messages = append(messages, llm.Message{
			Role:    "user",
			Content: fmt.Sprintf("Tool Output (%s):\n%s", pendingDecision.ToolName, toolOutput),
		})
	}

	for iteration < a.maxSteps {
		iteration++

		planCtx, planSpan := tracer.StartSpan(ctx, "step.plan",
			trace.Attribute{Key: "job_id", Value: job.ID.String()},
			trace.Attribute{Key: "step_number", Value: currentStepNum + 1},
			trace.Attribute{Key: "worker_id", Value: workerID},
		)

		planStart := a.clk.Now()
		resp, err := a.completeAndRecord(planCtx, s, job.ID, workerID, llm.CompleteRequest{
			Messages: messages,
			JSON:     true,
		})
		if err != nil {
			planSpan.SetStatus("error", err)
			planSpan.End()
			return fmt.Errorf("llm complete error at iteration %d: %w", iteration, err)
		}

		decision, parseErr := parseDecision(resp.Content)
		if parseErr != nil {
			nudge := append(messages,
				llm.Message{Role: "assistant", Content: resp.Content},
				llm.Message{Role: "user", Content: `Your response was not valid JSON matching the required schema. Return ONLY a JSON object with "thought" and "action" fields, where "action" is "tool" or "finish".`},
			)
			nudgeResp, retryErr := a.completeAndRecord(planCtx, s, job.ID, workerID, llm.CompleteRequest{
				Messages: nudge,
				JSON:     true,
			})
			if retryErr != nil {
				planSpan.SetStatus("error", retryErr)
				planSpan.End()
				return fmt.Errorf("llm nudge retry error: %w", retryErr)
			}
			decision, parseErr = parseDecision(nudgeResp.Content)
			if parseErr != nil {
				planSpan.SetStatus("error", parseErr)
				planSpan.End()
				return fmt.Errorf("invalid decision JSON after nudge retry: %w", parseErr)
			}
			resp.Content = nudgeResp.Content
		}

		currentStepNum++
		if _, err := s.RecordStep(planCtx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: currentStepNum,
			StepType:   StepTypePlan,
			Output:     json.RawMessage(resp.Content),
			WorkerID:   workerID,
		}); err != nil {
			planSpan.SetStatus("error", err)
			planSpan.End()
			return fmt.Errorf("record plan step %d: %w", currentStepNum, err)
		}
		planDur := a.clk.Now().Sub(planStart)
		planSpan.End()

		if a.metrics != nil {
			a.metrics.StepsTotal.WithLabelValues(StepTypePlan).Inc()
			a.metrics.StepDuration.WithLabelValues(StepTypePlan).Observe(planDur.Seconds())
		}

		messages = append(messages, llm.Message{Role: "assistant", Content: resp.Content})

		if decision.Action == "finish" {
			return nil
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		toolCtx, toolSpan := tracer.StartSpan(ctx, "step.tool_call",
			trace.Attribute{Key: "job_id", Value: job.ID.String()},
			trace.Attribute{Key: "step_number", Value: currentStepNum + 1},
			trace.Attribute{Key: "worker_id", Value: workerID},
		)
		toolStart := a.clk.Now()
		toolOutput := executeToolCall(toolCtx, a.registry, decision)
		currentStepNum++
		if _, err := s.RecordStep(toolCtx, job.ID, epoch, store.JobStep{
			JobID:      job.ID,
			StepNumber: currentStepNum,
			StepType:   StepTypeToolCall,
			Output:     toolOutputRaw(toolOutput),
			WorkerID:   workerID,
		}); err != nil {
			toolSpan.SetStatus("error", err)
			toolSpan.End()
			return fmt.Errorf("record tool_call step %d: %w", currentStepNum, err)
		}
		toolDur := a.clk.Now().Sub(toolStart)
		toolSpan.End()

		if a.metrics != nil {
			a.metrics.StepsTotal.WithLabelValues(StepTypeToolCall).Inc()
			a.metrics.StepDuration.WithLabelValues(StepTypeToolCall).Observe(toolDur.Seconds())
		}

		messages = append(messages, llm.Message{
			Role:    "user",
			Content: fmt.Sprintf("Tool Output (%s):\n%s", decision.ToolName, toolOutput),
		})
	}

	return fmt.Errorf("agent exceeded max steps limit of %d", a.maxSteps)
}

func (a *Agent) MaxSteps() int { return a.maxSteps }

func jobPrompt(payload json.RawMessage) string {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &p); err == nil && p.Prompt != "" {
		return p.Prompt
	}
	return string(payload)
}
