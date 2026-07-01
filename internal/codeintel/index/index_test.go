package index_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/index"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestIndexProject_EndToEnd proves the Phase 1 vertical: the indexer
// walks a temp project, parses with the default backend set (go/ast for
// Go), persists through the store seam, and the NATIVE engine answers
// SymbolsInFile / FindSymbols / Available / Stale from the index.
func TestIndexProject_EndToEnd(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	goSrc := `package sample

import "fmt"

// Greeter greets.
type Greeter struct{ name string }

func (g *Greeter) Hello() string { return fmt.Sprintf("hi %s", g.name) }

func Run() { g := &Greeter{}; _ = g.Hello() }
`
	writeFile(t, filepath.Join(root, "sample.go"), goSrc)
	// A file in an ignored dir must NOT be indexed.
	writeFile(t, filepath.Join(root, "node_modules", "junk.go"), "package junk\nfunc Junk() {}\n")

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry()})
	rep, err := ix.IndexProject(ctx, root, true)
	if err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	if rep.Indexed != 1 {
		t.Fatalf("expected 1 indexed file (node_modules skipped), got %d (scanned=%d)", rep.Indexed, rep.Scanned)
	}

	eng := codeintel.NewEngine(st)
	if !eng.Available() {
		t.Fatal("native engine reports unavailable after indexing")
	}

	abs := filepath.Join(root, "sample.go")
	syms, err := eng.SymbolsInFile(ctx, abs)
	if err != nil {
		t.Fatalf("SymbolsInFile: %v", err)
	}
	got := map[string]codeintel.Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}
	for _, want := range []struct {
		name, kind string
	}{
		{"Greeter", "class"},
		{"Hello", "method"},
		{"Run", "function"},
	} {
		s, ok := got[want.name]
		if !ok {
			t.Errorf("missing symbol %q; got %v", want.name, keys(got))
			continue
		}
		if s.Kind != want.kind {
			t.Errorf("%s: kind = %q, want %q", want.name, s.Kind, want.kind)
		}
		if s.EndLine < s.StartLine || s.EndLine == 0 {
			t.Errorf("%s: expected exact span (start=%d end=%d)", want.name, s.StartLine, s.EndLine)
		}
	}

	// FindSymbols name lookup.
	matches, err := eng.FindSymbols(ctx, abs, "Hello", "", "")
	if err != nil {
		t.Fatalf("FindSymbols: %v", err)
	}
	if len(matches) != 1 || matches[0].Name != "Hello" || matches[0].Kind != "method" {
		t.Fatalf("FindSymbols(Hello) = %+v, want one method match", matches)
	}

	// Incremental: re-indexing unchanged content does no work.
	rep2, err := ix.IndexProject(ctx, root, true)
	if err != nil {
		t.Fatalf("re-IndexProject: %v", err)
	}
	if rep2.Indexed != 0 || rep2.Unchanged != 1 {
		t.Fatalf("incremental: expected 0 indexed / 1 unchanged, got indexed=%d unchanged=%d", rep2.Indexed, rep2.Unchanged)
	}
}

// TestIndexProject_ConsentGate proves a new project over the limit is
// not indexed without consent (force=false).
func TestIndexProject_ConsentGate(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "package a\nfunc A() {}\n")
	writeFile(t, filepath.Join(root, "b.go"), "package a\nfunc B() {}\n")

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry(), AutoIndexLimit: 1})
	rep, err := ix.IndexProject(ctx, root, false)
	if err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	if !rep.NeedsConsent || rep.Indexed != 0 {
		t.Fatalf("expected needs-consent with 0 indexed, got %+v", rep)
	}
	if eng := codeintel.NewEngine(st); eng.Available() {
		t.Fatal("engine should be unavailable — nothing indexed under the consent gate")
	}
}

// TestIndexProject_Graph proves Phase 3: imports persist + queryable,
// and name-matched CALLS resolve across files (Run in b.go calls Helper
// in a.go), exposed via the native engine's traversal surface.
func TestIndexProject_Graph(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	st := store.New(database)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), `package p

import "fmt"

func Helper() string { return fmt.Sprintf("x") }
`)
	writeFile(t, filepath.Join(root, "b.go"), `package p

func Run() {
	_ = Helper()
	_ = Helper()
}
`)

	ix := index.New(index.Options{Store: st, Registry: index.DefaultRegistry()})
	if _, err := ix.IndexProject(ctx, root, true); err != nil {
		t.Fatalf("IndexProject: %v", err)
	}
	eng := codeintel.NewEngine(st)
	aPath := filepath.Join(root, "a.go")
	bPath := filepath.Join(root, "b.go")

	// Imports.
	imps, _ := eng.ImportsInFile(ctx, aPath)
	if len(imps) != 1 || imps[0] != "fmt" {
		t.Fatalf("ImportsInFile(a.go) = %v, want [fmt]", imps)
	}

	// Resolve symbol ids.
	helper := findOne(t, ctx, eng, aPath, "Helper")
	run := findOne(t, ctx, eng, bPath, "Run")

	// Run calls Helper (cross-file, name-matched).
	callees, _ := eng.CalleesOfSymbol(ctx, run.ID, 10)
	if !refsContain(callees, "Helper") {
		t.Fatalf("CalleesOfSymbol(Run) = %v, want it to include Helper", refNames(callees))
	}
	callers, _ := eng.CallersOfSymbol(ctx, helper.ID, 10)
	if !refsContain(callers, "Run") {
		t.Fatalf("CallersOfSymbol(Helper) = %v, want it to include Run", refNames(callers))
	}
	if n, _ := eng.CountCallers(ctx, helper.ID); n < 1 {
		t.Fatalf("CountCallers(Helper) = %d, want >=1", n)
	}

	// Reachable: Run -> Helper within depth 2.
	reached, _, _ := eng.Reachable(ctx, run.ID, codeintel.RelationCallees, 2, 50)
	if !refsContain(reached, "Helper") {
		t.Fatalf("Reachable(Run, callees) = %v, want it to include Helper", refNames(reached))
	}
	if total, _ := eng.CountEdgesByKind(ctx, "CALLS"); total < 1 {
		t.Fatalf("CountEdgesByKind(CALLS) = %d, want >=1", total)
	}
}

func findOne(t *testing.T, ctx context.Context, eng codeintel.Provider, path, name string) codeintel.SymbolMatch {
	t.Helper()
	ms, err := eng.FindSymbols(ctx, path, name, "", "")
	if err != nil || len(ms) == 0 {
		t.Fatalf("FindSymbols(%s, %s) = %v, %v", path, name, ms, err)
	}
	return ms[0]
}

func refsContain(refs []codeintel.Ref, name string) bool {
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func refNames(refs []codeintel.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func keys(m map[string]codeintel.Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
