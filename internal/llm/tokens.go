package llm

import "unicode/utf8"

// EstimateTokens calculates a conservative token count for a prompt.
// Heuristic: ~4 characters per token + fixed overhead for system prompt, model framing, and output headroom.
func EstimateTokens(req CompleteRequest) int {
	charCount := 0

	for _, msg := range req.Messages {
		charCount += utf8.RuneCountInString(msg.Role) + utf8.RuneCountInString(msg.Content)
	}

	// 1 token ≈ 4 characters
	estimatedTokens := charCount / 4

	// Add overhead for message formatting, tools framing, and max expected completion buffer (e.g. 500 tokens)
	overhead := 500

	total := estimatedTokens + overhead
	if total < 100 {
		return 100 // Floor minimum estimate
	}
	return total
}