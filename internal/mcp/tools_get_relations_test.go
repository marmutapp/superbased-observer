package mcp

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/codegraph"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/mcp/audit"
)

// getRelationsFixture wires the get_relations MCP tool against a
// real codegraph DB seeded inline. Mirrors getSymbolsFixture's shape.
type getRelationsFixture struct {
	s       *Server
	root    string
	auditDB *sql.DB
}

func newGetRelationsFixture(t *testing.T, seedSQL []string, opts GetRelationsOptions) *getRelationsFixture {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	auditW := audit.NewSQLWriter(database, slog.New(slog.NewTextHandler(io.Discard, nil)), audit.SQLWriterOptions{
		FlushInterval: 10 * time.Millisecond,
		BatchSize:     8,
	})
	t.Cleanup(func() { _ = auditW.Close() })

	root := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make sure a stub file exists for path-safety check.
	if err := os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cgPath := filepath.Join(tmp, "graph.db")
	cgDB, err := sql.Open("sqlite", "file:"+cgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range seedSQL {
		if _, err := cgDB.Exec(q); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, q)
		}
	}
	cgDB.Close()

	cg, err := codegraph.Open(cgPath)
	if err != nil {
		t.Fatalf("codegraph.Open: %v", err)
	}
	t.Cleanup(func() { _ = cg.Close() })

	s, err := New(Options{
		DB:                  database,
		ServerName:          "test",
		ServerVersion:       "0",
		CodegraphClient:     cg,
		AuditWriter:         auditW,
		GetRelationsEnabled: true,
		GetRelations:        opts,
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	return &getRelationsFixture{s: s, root: root, auditDB: database}
}

// callsCycleSeed returns the seed SQL for the A→B→C→A + A→D cycle
// graph. file_path is set to "<ROOT>/graph.ts" via the caller because
// it depends on the per-test temp root — we substitute it in.
func callsCycleSeed(root string) []string {
	graphFile := filepath.Join(root, "graph.ts")
	return []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, fqn TEXT, language TEXT)`,
		`CREATE TABLE edges (id INTEGER PRIMARY KEY, source_id INTEGER, target_id INTEGER, kind TEXT)`,
		`INSERT INTO nodes VALUES (1, 'function', 'A', '` + graphFile + `', 10, 20, 'A', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'function', 'B', '` + graphFile + `', 30, 40, 'B', 'typescript')`,
		`INSERT INTO nodes VALUES (3, 'function', 'C', '` + graphFile + `', 50, 60, 'C', 'typescript')`,
		`INSERT INTO nodes VALUES (4, 'function', 'D', '` + graphFile + `', 70, 80, 'D', 'typescript')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 2, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (2, 3, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (3, 1, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 4, 'CALLS')`,
	}
}

func defaultGetRelationsOpts() GetRelationsOptions {
	return GetRelationsOptions{
		AllowExtensions: []string{"ts", "tsx", "go"},
		MaxDepth:        5,
		MaxResults:      100,
	}
}

func TestGetRelations_CalleesHappyPath(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	// Override fixture root to use our pre-built path.
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "callees",
		"depth":        1,
	})
	if !res["ok"].(bool) {
		t.Fatalf("ok=false: %+v", res)
	}
	if res["kind"] != "callees" || int(res["depth"].(float64)) != 1 {
		t.Errorf("kind/depth: %+v", res)
	}
	results := res["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("want 2 callees (B, D), got %d: %+v", len(results), results)
	}
	r0 := results[0].(map[string]any)
	sym := r0["symbol"].(map[string]any)
	if sym["name"] != "B" || int(r0["depth"].(float64)) != 1 || r0["via_edge"] != "CALLS" {
		t.Errorf("result[0]: %+v", r0)
	}
}

func TestGetRelations_CallersHappyPath(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "callers",
	})
	// Default depth = 1; only C calls A.
	results := res["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["symbol"].(map[string]any)["name"] != "C" {
		t.Errorf("callers: %+v", results)
	}
}

func TestGetRelations_ContainsHappyPath(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "mod.ts"), []byte("// stub\n"), 0o600)
	graphFile := filepath.Join(root, "mod.ts")
	seed := []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, fqn TEXT, language TEXT)`,
		`CREATE TABLE edges (id INTEGER PRIMARY KEY, source_id INTEGER, target_id INTEGER, kind TEXT)`,
		`INSERT INTO nodes VALUES (1, 'module', 'pkg', '` + graphFile + `', 1, 100, 'pkg', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'class', 'Foo', '` + graphFile + `', 10, 50, 'pkg.Foo', 'typescript')`,
		`INSERT INTO nodes VALUES (3, 'method', 'bar', '` + graphFile + `', 20, 30, 'pkg.Foo.bar', 'typescript')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 2, 'CONTAINS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (2, 3, 'CONTAINS')`,
	}
	f := newGetRelationsFixture(t, seed, defaultGetRelationsOpts())
	f.root = root

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "mod.ts",
		"name":         "pkg",
		"kind":         "contains",
		"depth":        5,
	})
	results := res["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("contains: got %d, want 2 (Foo, bar): %+v", len(results), results)
	}
	if deg, _ := res["degraded"].(bool); deg {
		t.Errorf("contains with populated edges should NOT be degraded: %+v", res)
	}
}

func TestGetRelations_AmbiguousAnchor_RequiresFQN(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)
	graphFile := filepath.Join(root, "graph.ts")
	seed := []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, fqn TEXT, language TEXT)`,
		`CREATE TABLE edges (id INTEGER PRIMARY KEY, source_id INTEGER, target_id INTEGER, kind TEXT)`,
		`INSERT INTO nodes VALUES (1, 'function', 'handleClick', '` + graphFile + `', 50, 80, 'handleClick', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'method', 'handleClick', '` + graphFile + `', 200, 210, 'Editor.handleClick', 'typescript')`,
	}
	f := newGetRelationsFixture(t, seed, defaultGetRelationsOpts())
	f.root = root

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "handleClick",
		"kind":         "callers",
	})
	if res["ok"].(bool) {
		t.Errorf("ambiguous anchor should land ok=false: %+v", res)
	}
	if !strings.Contains(res["reason"].(string), "ambiguous anchor") {
		t.Errorf("reason: %q", res["reason"])
	}
	candidates := res["candidates"].([]any)
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(candidates))
	}
	fqns := map[string]bool{
		candidates[0].(map[string]any)["fqn"].(string): true,
		candidates[1].(map[string]any)["fqn"].(string): true,
	}
	if !fqns["handleClick"] || !fqns["Editor.handleClick"] {
		t.Errorf("candidates missing expected fqns: %+v", candidates)
	}
}

func TestGetRelations_FQNDisambiguation_Resolves(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)
	graphFile := filepath.Join(root, "graph.ts")
	seed := []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, fqn TEXT, language TEXT)`,
		`CREATE TABLE edges (id INTEGER PRIMARY KEY, source_id INTEGER, target_id INTEGER, kind TEXT)`,
		`INSERT INTO nodes VALUES (1, 'function', 'handleClick', '` + graphFile + `', 50, 80, 'handleClick', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'method', 'handleClick', '` + graphFile + `', 200, 210, 'Editor.handleClick', 'typescript')`,
		`INSERT INTO nodes VALUES (3, 'function', 'caller', '` + graphFile + `', 220, 230, 'caller', 'typescript')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (3, 2, 'CALLS')`, // caller → Editor.handleClick
	}
	f := newGetRelationsFixture(t, seed, defaultGetRelationsOpts())
	f.root = root

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "handleClick",
		"fqn":          "Editor.handleClick",
		"kind":         "callers",
	})
	if !res["ok"].(bool) {
		t.Fatalf("fqn-pinned should resolve, got: %+v", res)
	}
	results := res["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["symbol"].(map[string]any)["name"] != "caller" {
		t.Errorf("expected single 'caller' result, got %+v", results)
	}
}

func TestGetRelations_DefaultDepthIsOne(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	// Omit depth.
	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "callees",
	})
	if int(res["depth"].(float64)) != 1 {
		t.Errorf("default depth should be 1, got %v", res["depth"])
	}
	if len(res["results"].([]any)) != 2 {
		t.Errorf("depth-1 from A should reach 2 nodes (B, D)")
	}
}

func TestGetRelations_DepthClampedToMax(t *testing.T) {
	opts := defaultGetRelationsOpts()
	opts.MaxDepth = 2
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), opts)
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "callees",
		"depth":        99,
	})
	if int(res["depth"].(float64)) != 2 {
		t.Errorf("depth=99 with MaxDepth=2 should clamp to 2, got %v", res["depth"])
	}
}

func TestGetRelations_InvalidKind(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	errText := callToolExpectError(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "bogus",
	})
	if !strings.Contains(errText, "invalid kind") {
		t.Errorf("expected invalid-kind error, got %q", errText)
	}
}

func TestGetRelations_PathDenied(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	f.root = root

	// Point outside the project root.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "out.ts"), []byte("// x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         filepath.Join(other, "out.ts"),
		"name":         "A",
		"kind":         "callees",
	})
	if res["ok"].(bool) {
		t.Errorf("path outside root should land ok=false")
	}
	if !strings.Contains(res["reason"].(string), "outside project_root") {
		t.Errorf("reason: %q", res["reason"])
	}
}

func TestGetRelations_CodegraphUnavailable(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "obs.db")
	database, _ := db.Open(context.Background(), db.Options{Path: dbPath})
	t.Cleanup(func() { database.Close() })

	auditW := audit.NewSQLWriter(database, slog.New(slog.NewTextHandler(io.Discard, nil)), audit.SQLWriterOptions{FlushInterval: 10 * time.Millisecond})
	t.Cleanup(func() { _ = auditW.Close() })

	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "x.ts"), []byte("// x\n"), 0o600)

	s, _ := New(Options{
		DB:                  database,
		ServerName:          "test",
		ServerVersion:       "0",
		CodegraphClient:     codegraph.NewMissing(),
		AuditWriter:         auditW,
		GetRelationsEnabled: true,
		GetRelations:        defaultGetRelationsOpts(),
	})
	res := callTool(t, s, "get_relations", map[string]any{
		"project_root": root,
		"file":         "x.ts",
		"name":         "anything",
		"kind":         "callees",
	})
	if deg, _ := res["degraded"].(bool); !deg {
		t.Errorf("expected degraded=true, got %+v", res)
	}
	if !strings.Contains(res["reason"].(string), "codegraph unavailable") {
		t.Errorf("reason: %q", res["reason"])
	}
	// V7-17: warnings should include codegraph_unavailable in-band.
	warnings, _ := res["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if w == "codegraph_unavailable" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected codegraph_unavailable in warnings, got %v", warnings)
	}
}

func TestGetRelations_ContainsEmpty_HintsUpstream(t *testing.T) {
	// Seed a graph with CALLS edges only — no CONTAINS at all.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "contains",
	})
	if !res["ok"].(bool) {
		t.Fatalf("ok should stay true even when degraded, got %+v", res)
	}
	if deg, _ := res["degraded"].(bool); !deg {
		t.Errorf("CONTAINS empty should be degraded, got %+v", res)
	}
	if !strings.Contains(res["reason"].(string), "not populated") {
		t.Errorf("reason should mention upstream not populating, got %q", res["reason"])
	}
}

func TestGetRelations_TruncationFlag(t *testing.T) {
	opts := defaultGetRelationsOpts()
	opts.MaxResults = 2 // force truncation
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), opts)
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	res := callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "callees",
		"depth":        5,
	})
	if trunc, _ := res["truncated"].(bool); !trunc {
		t.Errorf("expected truncated=true at MaxResults=2, got %+v", res)
	}
	if len(res["results"].([]any)) != 2 {
		t.Errorf("expected 2 results, got %d", len(res["results"].([]any)))
	}
}

func TestGetRelations_AuditRowPerCall(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(root, 0o755)
	f := newGetRelationsFixture(t, callsCycleSeed(root), defaultGetRelationsOpts())
	f.root = root
	_ = os.WriteFile(filepath.Join(root, "graph.ts"), []byte("// stub\n"), 0o600)

	_ = callTool(t, f.s, "get_relations", map[string]any{
		"project_root": f.root,
		"file":         "graph.ts",
		"name":         "A",
		"kind":         "callees",
		"session_id":   "sess-relations",
	})
	if got := auditRowCount(t, f.auditDB,
		"tool_name = 'get_relations' AND session_id = ?", "sess-relations"); got != 1 {
		t.Errorf("expected 1 audit row, got %d", got)
	}
}

func TestGetRelations_NotRegisteredWhenDisabled(t *testing.T) {
	tmp := t.TempDir()
	database, _ := db.Open(context.Background(), db.Options{Path: filepath.Join(tmp, "obs.db")})
	t.Cleanup(func() { database.Close() })
	s, _ := New(Options{
		DB: database, ServerName: "test", ServerVersion: "0",
		GetRelationsEnabled: false,
	})
	resp := rpcCall(t, s, "tools/list", 1, nil)
	for _, raw := range resp["result"].(map[string]any)["tools"].([]any) {
		if raw.(map[string]any)["name"] == "get_relations" {
			t.Errorf("get_relations registered even though disabled")
		}
	}
}
