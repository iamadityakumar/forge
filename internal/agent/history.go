package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"forge/internal/llm"
	"forge/internal/store"
	"forge/internal/tools"
)

// reconstructHistory replays committed job_steps into an llm conversation. It
// returns:
//   - messages       — the conversation up to the last committed point
//   - pendingDecision — non-nil when the committed history leaves work to finish:
//       - Action == "tool": a lone trailing plan row whose tool_call was never
//         committed (worker killed mid-iteration). The caller must re-execute
//         the tool WITHOUT re-calling the LLM.
//       - Action == "finish": the final finish plan is already committed but the
//         job was never transitioned to completed (crash between the last plan
//         and the worker-loop CompleteJob). The caller should just return nil.
//   - completedIterations — the number of full plan+tool iterations already done
func reconstructHistory(steps []store.JobStep, systemPrompt, prompt string) ([]llm.Message, *Decision, int, error) {
	sort.Slice(steps, func(i, j int) bool {
		return steps[i].StepNumber < steps[j].StepNumber
	})

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}

	var pendingDecision *Decision
	completedIterations := 0

	for i := 0; i < len(steps); i++ {
		step := steps[i]

		if step.StepType != StepTypePlan {
			return nil, nil, 0, fmt.Errorf("corrupt history at step %d: unexpected step_type %q", step.StepNumber, step.StepType)
		}

		planContent := string(step.Output)
		d, err := parseDecision(planContent)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("corrupt history at step %d (plan): %w", step.StepNumber, err)
		}

		messages = append(messages, llm.Message{Role: "assistant", Content: planContent})

		if d.Action == "finish" {
			completedIterations++
			return messages, d, completedIterations, nil
		}

		if i == len(steps)-1 {
			// Trailing lone plan: the tool_call for this iteration was never
			// committed — the worker was killed right after recording the plan.
			pendingDecision = d
			completedIterations++
			break
		}

		nextStep := steps[i+1]
		if nextStep.StepType != StepTypeToolCall {
			return nil, nil, 0, fmt.Errorf("corrupt history at step %d: expected tool_call after plan, got %q", step.StepNumber, nextStep.StepType)
		}

		toolOutput := decodeToolOutput(nextStep.Output)
		messages = append(messages, llm.Message{
			Role:    "user",
			Content: fmt.Sprintf("Tool Output (%s):\n%s", d.ToolName, toolOutput),
		})
		i++ // consume the tool_call row
		completedIterations++
	}

	return messages, pendingDecision, completedIterations, nil
}

// executeToolCall runs a decision's tool through the registry and returns its
// raw text output (or an error string as output — tool errors never crash the
// loop; they become observations the model can react to).
func executeToolCall(ctx context.Context, reg *tools.Registry, d *Decision) string {
	if reg == nil {
		return "Error: no tool registry configured"
	}

	tool, ok := reg.Get(d.ToolName)
	if !ok {
		return fmt.Sprintf("Error: tool '%s' not registered", d.ToolName)
	}

	argsJSON := string(d.ToolArgs)
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}

	out, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		return fmt.Sprintf("Tool '%s' execution error: %v", d.ToolName, err)
	}

	return out
}

// toolOutputRaw encodes a tool's raw text output as a JSON value so it can be
// stored in the json.RawMessage job_steps.output column unconditionally.
func toolOutputRaw(output string) json.RawMessage {
	b, err := json.Marshal(output)
	if err != nil {
		return json.RawMessage(`"<unserializable tool output>"`)
	}
	return json.RawMessage(b)
}

// decodeToolOutput inverts toolOutputRaw (and tolerates legacy raw text).
func decodeToolOutput(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}