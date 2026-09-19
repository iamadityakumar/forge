package tools

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"forge/internal/llm"
	"forge/internal/metrics"
	"forge/internal/store"
)

//go:embed kb/*.md
var kbFS embed.FS

type SearchKBTool struct {
	store     *store.PgStore
	embedding llm.EmbeddingBackend
	metrics   *metrics.Metrics
}

func NewSearchKBTool() *SearchKBTool {
	return &SearchKBTool{}
}

func NewVectorSearchKBTool(s *store.PgStore, embedding llm.EmbeddingBackend, m *metrics.Metrics) *SearchKBTool {
	return &SearchKBTool{store: s, embedding: embedding, metrics: m}
}

func (s *SearchKBTool) isVectorSearchEnabled() bool {
	return s.store != nil && s.embedding != nil
}

func (s *SearchKBTool) Name() string {
	return "search_kb"
}

func (s *SearchKBTool) Description() string {
	return "Search competitive programming knowledge base strategies (prefix_sum, two_pointers, sliding_window) by query keyword."
}

func (s *SearchKBTool) Schema() string {
	return `{"type":"object","properties":{"query":{"type":"string","description":"Keyword to search for in knowledge base strategy documents"}},"required":["query"]}`
}

type searchKBArgs struct {
	Query string `json:"query"`
}

func (s *SearchKBTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args searchKBArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	query := strings.ToLower(strings.TrimSpace(args.Query))
	if query == "" {
		return "Query is empty. Please provide a search keyword.", nil
	}
	if s.store != nil && s.embedding != nil {
		started := time.Now()
		vector, err := s.embedding.Embed(ctx, query)
		if err != nil {
			return "", fmt.Errorf("embed query: %w", err)
		}
		chunks, err := s.store.SearchKB(ctx, vector, 5)
		if err != nil {
			return "", err
		}
		if s.metrics != nil {
			s.metrics.RetrievalLatency.Observe(time.Since(started).Seconds())
		}
		if len(chunks) == 0 {
			return fmt.Sprintf("No knowledge base documents found matching query: %s", args.Query), nil
		}
		var results []string
		for _, chunk := range chunks {
			results = append(results, fmt.Sprintf("=== Source: %s (chunk %d) ===\n%s", chunk.Source, chunk.ChunkNumber, chunk.Content))
		}
		return strings.Join(results, "\n\n"), nil
	}

	var results []string

	err := fs.WalkDir(kbFS, "kb", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := kbFS.ReadFile(path)
		if err != nil {
			return nil
		}

		content := string(data)
		if strings.Contains(strings.ToLower(content), query) || strings.Contains(strings.ToLower(path), query) {
			results = append(results, fmt.Sprintf("=== File: %s ===\n%s", path, content))
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to search kb: %w", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No knowledge base documents found matching query: %s", args.Query), nil
	}

	return strings.Join(results, "\n\n"), nil
}
