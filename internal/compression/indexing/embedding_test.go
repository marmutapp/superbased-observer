package indexing

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
)

func TestVectorize_Shape(t *testing.T) {
	vec := vectorize("hello world this is a test")
	if len(vec) != vectorDim {
		t.Fatalf("vec length: %d want %d", len(vec), vectorDim)
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if math.Abs(norm-1.0) > 0.001 {
		t.Errorf("norm: %f want 1.0", norm)
	}
}

func TestVectorize_EmptyText(t *testing.T) {
	vec := vectorize("")
	for _, v := range vec {
		if v != 0 {
			t.Fatal("empty text should produce zero vector")
		}
	}
}

func TestCosine_Identical(t *testing.T) {
	a := vectorize("the quick brown fox")
	sim := cosine(a, a)
	if math.Abs(sim-1.0) > 0.001 {
		t.Errorf("cosine(a,a): %f want 1.0", sim)
	}
}

func TestCosine_Similar(t *testing.T) {
	a := vectorize("go test failed with error in package main")
	b := vectorize("go test error failure in main package")
	c := vectorize("deployed kubernetes cluster with helm chart")
	simAB := cosine(a, b)
	simAC := cosine(a, c)
	if simAB <= simAC {
		t.Errorf("similar texts (%.3f) should score higher than dissimilar (%.3f)", simAB, simAC)
	}
}

func TestEmbedAndSearch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	e := NewEmbedder(database)
	ctx := context.Background()

	items := []EmbedItem{
		{ActionID: 1, Text: "go test -race ./... failed with data race in handler"},
		{ActionID: 2, Text: "npm install completed successfully with 200 packages"},
		{ActionID: 3, Text: "go test error: undefined function in main package"},
	}
	if err := e.EmbedBatch(ctx, items); err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}

	results, err := e.SemanticSearch(ctx, "go test failure race condition", 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	// The first query result should be the most relevant Go test excerpt.
	topID := results[0].ActionID
	if topID != 1 && topID != 3 {
		t.Errorf("top result: action %d (expected 1 or 3 — Go test excerpts)", topID)
	}
	if results[0].Similarity <= 0 {
		t.Errorf("top similarity should be positive: %f", results[0].Similarity)
	}
}

func TestEmbed_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	e := NewEmbedder(database)
	ctx := context.Background()
	if err := e.Embed(ctx, 1, "original text"); err != nil {
		t.Fatal(err)
	}
	if err := e.Embed(ctx, 1, "updated text"); err != nil {
		t.Fatal(err)
	}
	var count int
	database.QueryRow("SELECT COUNT(*) FROM action_embeddings WHERE action_id = 1").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestPackUnpack_RoundTrip(t *testing.T) {
	vec := vectorize("round trip test data")
	blob := packVector(vec)
	got := unpackVector(blob)
	if got == nil {
		t.Fatal("unpack returned nil")
	}
	for i := range vec {
		if vec[i] != got[i] {
			t.Fatalf("mismatch at %d: %f != %f", i, vec[i], got[i])
		}
	}
}
