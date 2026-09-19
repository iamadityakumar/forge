package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"forge/internal/llm"
	"forge/internal/store"
)

func main() {
	directory := flag.String("dir", "internal/tools/kb", "directory containing Markdown KB documents")
	flag.Parse()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}
	pg, err := store.NewPgStore(databaseURL)
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
	entries, err := os.ReadDir(*directory)
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := *directory + string(os.PathSeparator) + entry.Name()
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		chunks := splitChunks(string(data), 1200, 200)
		for number, content := range chunks {
			vector, err := embedding.Embed(context.Background(), content)
			if err != nil {
				log.Fatalf("embed %s chunk %d: %v", entry.Name(), number, err)
			}
			if err := pg.UpsertKBChunk(context.Background(), entry.Name(), number, content, vector); err != nil {
				log.Fatal(err)
			}
		}
		fmt.Printf("ingested %s (%d chunks)\n", entry.Name(), len(chunks))
	}
}

func splitChunks(content string, size, overlap int) []string {
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 5
	}
	var chunks []string
	for start := 0; start < len(content); {
		end := start + size
		if end > len(content) {
			end = len(content)
		}
		if end < len(content) {
			if boundary := strings.LastIndexAny(content[start:end], " \n"); boundary > size/2 {
				end = start + boundary
			}
		}
		chunks = append(chunks, strings.TrimSpace(content[start:end]))
		if end == len(content) {
			break
		}
		start = end - overlap
	}
	return chunks
}
