package surface_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/analyze"
	"github.com/marmutapp/superbased-observer/internal/codeintel/index"
	"github.com/marmutapp/superbased-observer/internal/codeintel/surface"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestSurfaces_EndToEnd indexes a small fixture project (which runs the
// Phase 6 derived-build sweep: FTS + embeddings + MinHash) and exercises
// every Tier-C surface through the Service over the native engine.
func TestSurfaces_EndToEnd(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), `package p

func Helper() string { return "x" }

func Dup(x int) int { return x }

func unusedThing() { _ = 1 }
`)
	write(t, filepath.Join(root, "b.go"), `package p

func Run() {
	_ = Helper()
	_ = Helper()
}

func Dup(x int) int { return x }
`)

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry()})
	if _, err := ix.IndexProject(ctx, root, true); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	svc := surface.New(codeintel.NewEngine(st))
	if !svc.Available() {
		t.Fatal("service unavailable after indexing")
	}

	// --- search (FTS) ---
	hits, err := svc.Search(ctx, root, "helper", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !containsName(symNames(hits), "Helper") {
		t.Errorf("Search(helper) = %v, want it to include Helper", symNames(hits))
	}

	// Resolve Helper's id for the graph surfaces.
	eng := codeintel.NewEngine(st)
	helper := find(t, ctx, eng, filepath.Join(root, "a.go"), "Helper")

	// --- architecture ---
	arch, err := svc.Architecture(ctx, root)
	if err != nil {
		t.Fatalf("Architecture: %v", err)
	}
	if arch.TotalNodes == 0 || len(arch.Dirs) == 0 {
		t.Errorf("Architecture empty: %+v", arch)
	}
	if arch.TotalEdges < 1 {
		t.Errorf("Architecture should see the Run->Helper CALLS edge, got %d", arch.TotalEdges)
	}

	// --- dead code ---
	dead, err := svc.DeadCode(ctx, root, true) // only unexported
	if err != nil {
		t.Fatalf("DeadCode: %v", err)
	}
	if !containsDead(dead, "unusedThing") {
		t.Errorf("DeadCode(onlyUnexported) = %v, want it to include unusedThing", deadNames(dead))
	}
	// Helper is called, so it must NOT be dead.
	if containsDead(dead, "Helper") {
		t.Errorf("Helper is called; should not be dead")
	}

	// --- impact ---
	impacted, err := svc.Impact(ctx, root, []string{"Helper"})
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	if !containsName(symNames(impacted), "Run") {
		t.Errorf("Impact(Helper) = %v, want it to include Run", symNames(impacted))
	}

	// --- similar (near-clone) ---
	// The two Dup definitions are byte-identical → identical token sets →
	// share all MinHash bands, so each is the other's similar candidate.
	dups, err := eng.FindSymbols(ctx, filepath.Join(root, "a.go"), "Dup", "", "")
	if err != nil || len(dups) == 0 {
		t.Fatalf("FindSymbols(Dup): %v / %v", dups, err)
	}
	sim, err := svc.Similar(ctx, dups[0].ID, 10)
	if err != nil {
		t.Fatalf("Similar: %v", err)
	}
	if !containsName(symNames(sim), "Dup") {
		t.Errorf("Similar(Dup) = %v, want the other Dup clone", symNames(sim))
	}

	// --- semantic related (just exercise; cosine over a tiny corpus) ---
	if _, err := svc.SemanticNeighbors(ctx, helper.ID, 5); err != nil {
		t.Fatalf("SemanticNeighbors: %v", err)
	}

	// --- query (Cypher subset) ---
	rs, err := svc.Query(ctx, root, `MATCH (a)-[:CALLS]->(b) WHERE b.name = "Helper" RETURN a.name`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rs.Columns) != 1 || rs.Columns[0] != "a.name" {
		t.Errorf("query columns = %v", rs.Columns)
	}
	foundRun := false
	for _, row := range rs.Rows {
		if len(row) == 1 && row[0] == "Run" {
			foundRun = true
		}
	}
	if !foundRun {
		t.Errorf("query rows = %v, want a row [Run]", rs.Rows)
	}
}

func find(t *testing.T, ctx context.Context, eng codeintel.Provider, path, name string) codeintel.SymbolMatch {
	t.Helper()
	ms, err := eng.FindSymbols(ctx, path, name, "", "")
	if err != nil || len(ms) == 0 {
		t.Fatalf("FindSymbols(%s,%s): %v / %v", path, name, ms, err)
	}
	return ms[0]
}

func symNames(ms []codeintel.SymbolMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

func containsName(names []string, want string) bool {
	return slices.Contains(names, want)
}

func containsDead(ds []analyze.DeadSymbol, name string) bool {
	for _, d := range ds {
		if d.Name == name {
			return true
		}
	}
	return false
}

func deadNames(ds []analyze.DeadSymbol) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
