package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"forge/internal/llm"
	"forge/internal/store"
	"forge/internal/tools"
)

type dataset struct {
	Tasks   []task  `json:"tasks"`
	Queries []query `json:"queries"`
}
type task struct {
	ID     string           `json:"id"`
	Prompt string           `json:"prompt"`
	Code   string           `json:"code"`
	Tests  []tools.TestCase `json:"tests"`
}
type query struct {
	Query          string `json:"query"`
	ExpectedSource string `json:"expected_source"`
}
type result struct {
	RecallAt1    float64 `json:"recall_at_1"`
	RecallAt3    float64 `json:"recall_at_3"`
	RecallAt5    float64 `json:"recall_at_5"`
	MRR          float64 `json:"mrr"`
	TaskPassRate float64 `json:"task_pass_rate"`
	Queries      int     `json:"queries"`
	Tasks        int     `json:"tasks"`
}

func main() {
	path := flag.String("dataset", "eval/rag_dataset.json", "evaluation dataset")
	output := flag.String("output", "eval-results.json", "result JSON path")
	flag.Parse()
	data, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	var input dataset
	if err := json.Unmarshal(data, &input); err != nil {
		log.Fatal(err)
	}
	if len(input.Queries) == 0 || len(input.Tasks) == 0 {
		log.Fatal("dataset must contain queries and tasks")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL is required")
	}
	pg, err := store.NewPgStore(url)
	if err != nil {
		log.Fatal(err)
	}
	defer pg.Close()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	model := os.Getenv("OLLAMA_EMBEDDING_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	embedding := llm.NewOllamaEmbeddingBackend(host, model, &http.Client{})
	ctx := context.Background()
	hits := [3]int{}
	var mrr float64
	for _, item := range input.Queries {
		vector, err := embedding.Embed(ctx, item.Query)
		if err != nil {
			log.Fatalf("embed %q: %v", item.Query, err)
		}
		chunks, err := pg.SearchKB(ctx, vector, 5)
		if err != nil {
			log.Fatal(err)
		}
		rank := 0
		for i, chunk := range chunks {
			if strings.EqualFold(chunk.Source, item.ExpectedSource) || strings.HasSuffix(chunk.Source, item.ExpectedSource) {
				rank = i + 1
				break
			}
		}
		if rank > 0 {
			mrr += 1 / float64(rank)
			for k := range hits {
				if rank <= k+1 {
					hits[k]++
				}
			}
		}
	}
	runner := tools.NewRunTestsTool()
	passed := 0
	for _, item := range input.Tasks {
		args, _ := json.Marshal(tools.RunTestsArgs{Code: item.Code, TestCases: item.Tests})
		raw, err := runner.Execute(ctx, string(args))
		if err != nil {
			log.Fatalf("task %s: %v", item.ID, err)
		}
		var outcome tools.RunTestsOutput
		if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
			log.Fatal(err)
		}
		if outcome.Success {
			passed++
		}
	}
	n := float64(len(input.Queries))
	taskN := float64(len(input.Tasks))
	out := result{RecallAt1: float64(hits[0]) / n, RecallAt3: float64(hits[1]) / n, RecallAt5: float64(hits[2]) / n, MRR: mrr / n, TaskPassRate: float64(passed) / taskN, Queries: len(input.Queries), Tasks: len(input.Tasks)}
	encoded, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(*output, encoded, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(encoded))
}
