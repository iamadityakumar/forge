package llm

import (
	"context"
	"testing"
)

func TestFakeEmbeddingBackendIsDeterministic(t *testing.T) {
	backend := NewFakeEmbeddingBackend(8)
	first, err := backend.Embed(context.Background(), "prefix sum")
	if err != nil {
		t.Fatal(err)
	}
	second, err := backend.Embed(context.Background(), "prefix sum")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 8 || len(second) != 8 {
		t.Fatalf("unexpected dimensions: %d, %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("vector changed at index %d", i)
		}
	}
}
