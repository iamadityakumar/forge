package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"forge/internal/tools"
)

type Decision struct {
	Thought  string          `json:"thought"`
	Action   string          `json:"action"` // "tool" or "finish"
	ToolName string          `json:"tool_name,omitempty"`
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`
	Answer   string          `json:"answer,omitempty"`
}

func parseDecision(raw string) (*Decision, error) {
	trimmed := strings.TrimSpace(raw)

	var d Decision
	if err := json.Unmarshal([]byte(trimmed), &d); err == nil {
		if err := validateDecision(&d); err == nil {
			return &d, nil
		}
	}

	extracted := extractJSONBlock(trimmed)
	if extracted != "" {
		if err := json.Unmarshal([]byte(extracted), &d); err == nil {
			if err := validateDecision(&d); err == nil {
				return &d, nil
			}
		}
	}

	return nil, fmt.Errorf("invalid decision JSON structure or schema")
}

func validateDecision(d *Decision) error {
	switch d.Action {
	case "tool":
		if strings.TrimSpace(d.ToolName) == "" {
			return fmt.Errorf("action 'tool' requires non-empty tool_name")
		}
		return nil
	case "finish":
		return nil
	default:
		return fmt.Errorf("unknown action '%s', must be 'tool' or 'finish'", d.Action)
	}
}

func extractJSONBlock(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}
	return ""
}

func buildSystemPrompt(registry *tools.Registry) string {
	var toolDescs []string
	if registry != nil {
		for _, t := range registry.List() {
			toolDescs = append(toolDescs, fmt.Sprintf("- **%s**: %s\n  Schema: %s", t.Name(), t.Description(), t.Schema()))
		}
	}

	toolsSection := "None registered."
	if len(toolDescs) > 0 {
		toolsSection = strings.Join(toolDescs, "\n\n")
	}

	return fmt.Sprintf(`You are a competitive programming assistant running in an automated execution engine.
Your goal is to solve the competitive programming task provided in the problem description.

You MUST respond strictly with a valid JSON object. Do NOT wrap output in extra conversational text outside the JSON.

### Available Tools:
%s

### Response Format:
To execute a tool, return JSON:
{
  "thought": "Your reasoning here...",
  "action": "tool",
  "tool_name": "<name_of_tool>",
  "tool_args": { ... arguments matching tool schema ... }
}

To finish and complete the task, return JSON:
{
  "thought": "Your reasoning here...",
  "action": "finish",
  "answer": "Your final detailed solution and explanation..."
}`, toolsSection)
}