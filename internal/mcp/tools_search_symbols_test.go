package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func newSearchStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	st := store.New(database)
	root := filepath.Join(t.TempDir(), "proj")
	seeds := []symbolSeed{{
		relPath: "proxy.go",
		nodes: []nodeSeed{
			{id: 1, kind: "function", name: "buildProxy", fqn: "buildProxy", startLine: 10, endLine: 40, language: "go"},
			{id: 2, kind: "function", name: "wireRouting", fqn: "wireRouting", startLine: 50, endLine: 70, language: "go"},
		},
	}}
	seedCodeIntel(t, st, root, seeds, nil)
	if err := st.CodeIntelBuildDerived(ctx, root); err != nil {
		t.Fatalf("CodeIntelBuildDerived: %v", err)
	}
	return st, root
}

// TestSearchSymbols_FindsSeededSymbol proves the tool returns a seeded
// symbol via the Tier-C FTS index, scoped to the project, with a computed
// project-relative path.
func TestSearchSymbols_FindsSeededSymbol(t *testing.T) {
	st, root := newSearchStore(t)
	tool := newSearchSymbolsTool(codeintel.NewEngine(st))

	raw, _ := json.Marshal(map[string]any{"query": "buildProxy", "project_root": root})
	res, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r, ok := res.(searchSymbolsResult)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if !r.OK || r.Degraded {
		t.Fatalf("ok=%v degraded=%v warnings=%v", r.OK, r.Degraded, r.Warnings)
	}
	found := false
	for _, h := range r.Results {
		if h.Name == "buildProxy" {
			found = true
			if h.ProjectRelativePath != "proxy.go" {
				t.Errorf("project_relative_path = %q, want proxy.go", h.ProjectRelativePath)
			}
			if h.Kind != "function" || h.StartLine != 10 {
				t.Errorf("unexpected metadata: %+v", h)
			}
		}
	}
	if !found {
		t.Fatalf("buildProxy not in results: %+v", r.Results)
	}
}

// TestSearchSymbols_DegradedWhenUnavailable proves the tool fails open with
// the index_unavailable warning rather than erroring.
func TestSearchSymbols_DegradedWhenUnavailable(t *testing.T) {
	tool := newSearchSymbolsTool(codeintel.Unavailable())
	raw, _ := json.Marshal(map[string]any{"query": "anything"})
	res, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(searchSymbolsResult)
	if !r.Degraded {
		t.Fatal("expected degraded when index unavailable")
	}
	if len(r.Warnings) == 0 || r.Warnings[0] != WarningIndexUnavailable {
		t.Errorf("warnings = %v, want %s", r.Warnings, WarningIndexUnavailable)
	}
}

// TestSearchSymbols_QueryRequired proves an empty query is rejected.
func TestSearchSymbols_QueryRequired(t *testing.T) {
	tool := newSearchSymbolsTool(codeintel.Unavailable())
	raw, _ := json.Marshal(map[string]any{"project_root": "/x"})
	if _, err := tool.Invoke(context.Background(), raw); err == nil {
		t.Fatal("expected error for missing query")
	}
}
