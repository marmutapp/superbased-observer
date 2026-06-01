package codegraph

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestNewMissingIsUnavailable(t *testing.T) {
	c := NewMissing()
	if c.Available() {
		t.Error("Available() must be false on a missing client")
	}
	if c.Path() != "" {
		t.Errorf("Path() must be empty: %q", c.Path())
	}
	for _, fn := range []func() ([]string, error){
		func() ([]string, error) { return c.FunctionsInFile(context.Background(), "x.go") },
		func() ([]string, error) { return c.ImportsInFile(context.Background(), "x.go") },
		func() ([]string, error) { return c.CallersOf(context.Background(), "Foo") },
	} {
		got, err := fn()
		if err != nil {
			t.Errorf("unavailable client returned error: %v", err)
		}
		if got != nil {
			t.Errorf("unavailable client returned non-nil: %v", got)
		}
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close on missing: %v", err)
	}
}

func TestOpenMissingPathStaysUnavailable(t *testing.T) {
	c, err := Open("/nonexistent/path/graph.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.Available() {
		t.Error("missing file should be unavailable, not error")
	}
	if c.Path() != "/nonexistent/path/graph.db" {
		t.Errorf("Path: %s", c.Path())
	}
}

func TestOpenWithSchema_ToleratesMissingTables(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE placeholder (x INT)`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Available() {
		t.Fatal("expected Available() after Open on existing file")
	}
	got, err := c.FunctionsInFile(context.Background(), "x.go")
	if err != nil {
		t.Errorf("FunctionsInFile errored on missing schema: %v", err)
	}
	if got != nil {
		t.Errorf("expected empty, got %v", got)
	}
}

// seedSchema creates a graph DB with the confirmed codebase-memory-mcp
// schema (nodes + edges) and returns the database handle.
var seedStatements = []string{
	`CREATE TABLE nodes (
		id INTEGER PRIMARY KEY,
		kind TEXT NOT NULL,
		name TEXT NOT NULL,
		file_path TEXT,
		start_line INTEGER,
		end_line INTEGER,
		fqn TEXT,
		language TEXT
	)`,
	`CREATE TABLE edges (
		id INTEGER PRIMARY KEY,
		source_id INTEGER NOT NULL,
		target_id INTEGER NOT NULL,
		kind TEXT NOT NULL
	)`,
	// Functions in x.go
	`INSERT INTO nodes (id, kind, name, file_path, start_line, end_line, fqn, language)
	 VALUES (1, 'function', 'Foo', 'x.go', 10, 20, 'pkg.Foo', 'go')`,
	`INSERT INTO nodes (id, kind, name, file_path, start_line, end_line, fqn, language)
	 VALUES (2, 'function', 'Bar', 'x.go', 30, 40, 'pkg.Bar', 'go')`,
	// Import target
	`INSERT INTO nodes (id, kind, name, file_path, start_line, end_line, fqn, language)
	 VALUES (3, 'module', 'fmt', '', 0, 0, 'fmt', 'go')`,
	// IMPORTS edge: node 1 (Foo in x.go) imports node 3 (fmt)
	`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 3, 'IMPORTS')`,
	// CALLS edge: node 2 (Bar) calls node 1 (Foo)
	`INSERT INTO edges (source_id, target_id, kind) VALUES (2, 1, 'CALLS')`,
}

func TestOpenAndQuery_WithRealSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range seedStatements {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	fns, err := c.FunctionsInFile(context.Background(), "x.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(fns) != 2 || fns[0] != "Foo" || fns[1] != "Bar" {
		t.Errorf("functions: %v", fns)
	}

	imps, err := c.ImportsInFile(context.Background(), "x.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(imps) != 1 || imps[0] != "fmt" {
		t.Errorf("imports: %v", imps)
	}

	callers, err := c.CallersOf(context.Background(), "Foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0] != "Bar" {
		t.Errorf("callers: %v", callers)
	}
}

func TestDataDir_IsHomeRelative(t *testing.T) {
	d := DataDir()
	if d == "" {
		t.Fatal("empty DataDir")
	}
	if !strings.HasSuffix(d, binaryName) {
		t.Errorf("DataDir doesn't end with %s: %s", binaryName, d)
	}
}

func TestFindProjectDB_MatchesProject(t *testing.T) {
	base := t.TempDir()
	projHash := "abcdef123456"
	dbDir := filepath.Join(base, projHash)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "graph.db")
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range seedStatements[:2] {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	projRoot := "/home/user/myproject"
	_, err = d.Exec(
		`INSERT INTO nodes (kind, name, file_path, start_line, end_line, language)
		 VALUES ('function', 'Main', ?, 1, 10, 'go')`,
		projRoot+"/main.go",
	)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	// probeProjectMatch should find it.
	if !probeProjectMatch(dbPath, projRoot) {
		t.Error("probeProjectMatch should return true")
	}
	if probeProjectMatch(dbPath, "/other/project") {
		t.Error("probeProjectMatch should return false for unrelated project")
	}
}

// TestInstall_EndToEnd spins up a test HTTP server that serves a fake
// GitHub release with a tar.gz archive containing a dummy binary.
func TestInstall_EndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only")
	}
	targetDir := filepath.Join(t.TempDir(), "bin")
	dummyBinary := []byte("#!/bin/sh\necho hello\n")

	archiveName := platformArchiveName()
	if archiveName == "" {
		t.Fatalf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	archiveBytes := buildTarGz(t, binaryName, dummyBinary)
	archiveHash := sha256hex(archiveBytes)
	checksums := fmt.Sprintf("%s  %s\n", archiveHash, archiveName)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			rel := releaseInfo{
				TagName: "v0.6.0",
				Assets: []releaseAsset{
					{Name: archiveName, BrowserDownloadURL: srv.URL + "/archive"},
					{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums"},
				},
			}
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/archive":
			w.Write(archiveBytes)
		case r.URL.Path == "/checksums":
			w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: rewriteTransport{base: srv.URL},
	}

	path, err := Install(context.Background(), InstallOptions{
		TargetDir:  targetDir,
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.HasSuffix(path, binaryName) {
		t.Errorf("path: %s", path)
	}
	// Verify the binary exists and is executable.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("binary is not executable")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(dummyBinary) {
		t.Errorf("binary content mismatch: got %q", got)
	}
}

func TestInstall_ChecksumMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only")
	}
	archiveName := platformArchiveName()
	archiveBytes := buildTarGz(t, binaryName, []byte("binary"))
	badChecksums := fmt.Sprintf("0000000000000000000000000000000000000000000000000000000000000000  %s\n", archiveName)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			rel := releaseInfo{
				TagName: "v0.6.0",
				Assets: []releaseAsset{
					{Name: archiveName, BrowserDownloadURL: srv.URL + "/archive"},
					{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums"},
				},
			}
			json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/archive":
			w.Write(archiveBytes)
		case r.URL.Path == "/checksums":
			w.Write([]byte(badChecksums))
		}
	}))
	defer srv.Close()

	_, err := Install(context.Background(), InstallOptions{
		TargetDir:  t.TempDir(),
		HTTPClient: &http.Client{Transport: rewriteTransport{base: srv.URL}},
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}

// rewriteTransport rewrites all requests to target the test server.
type rewriteTransport struct {
	base string
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.base, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

func buildTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	gw := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Size: int64(len(content)),
		Mode: 0o755,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	data, _ := os.ReadFile(tmp.Name())
	return data
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// seedSymbolGraph creates a graph DB and seeds it with a mix of
// kinds in editor.tsx for SymbolsInFile testing. Returns the
// dbPath so the test can also reach in to call Open / Stat.
func seedSymbolGraph(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stmts := []string{
		`CREATE TABLE nodes (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			file_path TEXT,
			start_line INTEGER,
			end_line INTEGER,
			fqn TEXT,
			language TEXT
		)`,
		// editor.tsx user-facing symbols (mixed kinds, out-of-order start_lines)
		`INSERT INTO nodes VALUES (1, 'class', 'Editor', 'editor.tsx', 120, 240, 'Editor', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'function', 'handleClick', 'editor.tsx', 50, 80, 'handleClick', 'typescript')`,
		`INSERT INTO nodes VALUES (3, 'method', 'render', 'editor.tsx', 200, 220, 'Editor.render', 'typescript')`,
		`INSERT INTO nodes VALUES (4, 'interface', 'EditorProps', 'editor.tsx', 10, 30, 'EditorProps', 'typescript')`,
		`INSERT INTO nodes VALUES (5, 'type', 'EditorState', 'editor.tsx', 35, 45, 'EditorState', 'typescript')`,
		// Noise kinds that MUST NOT appear in SymbolsInFile output.
		`INSERT INTO nodes VALUES (6, 'variable', 'editorRef', 'editor.tsx', 100, 100, 'editorRef', 'typescript')`,
		`INSERT INTO nodes VALUES (7, 'parameter', 'evt', 'editor.tsx', 51, 51, 'handleClick.evt', 'typescript')`,
		// Symbol in a different file — must NOT leak.
		`INSERT INTO nodes VALUES (8, 'function', 'unrelated', 'other.go', 1, 5, 'pkg.unrelated', 'go')`,
	}
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	return dbPath
}

// TestSymbolsInFile_ReturnsByStartLineAscending pins the v1.7.7
// determinism contract: the marker enrichment depends on the symbol
// list being byte-stable across repeated compressions, so the
// codegraph query MUST sort by start_line ASC and not depend on the
// SQLite row-insertion order.
func TestSymbolsInFile_ReturnsByStartLineAscending(t *testing.T) {
	c, err := Open(seedSymbolGraph(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	syms, err := c.SymbolsInFile(context.Background(), "editor.tsx")
	if err != nil {
		t.Fatalf("SymbolsInFile: %v", err)
	}
	wantOrder := []string{"EditorProps", "EditorState", "handleClick", "Editor", "render"}
	if len(syms) != len(wantOrder) {
		t.Fatalf("got %d symbols, want %d: %v", len(syms), len(wantOrder), syms)
	}
	for i, want := range wantOrder {
		if syms[i].Name != want {
			t.Errorf("symbol[%d]: got %q want %q (full: %v)", i, syms[i].Name, want, syms)
		}
	}
}

// TestSymbolsInFile_FiltersByKind pins that variable/parameter/etc
// noise kinds are excluded — the marker should carry structurally
// useful symbols only.
func TestSymbolsInFile_FiltersByKind(t *testing.T) {
	c, err := Open(seedSymbolGraph(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	syms, _ := c.SymbolsInFile(context.Background(), "editor.tsx")
	for _, s := range syms {
		if s.Kind == "variable" || s.Kind == "parameter" || s.Kind == "comment" {
			t.Errorf("noise kind leaked: %s (%s)", s.Name, s.Kind)
		}
	}
}

// TestSymbolsInFile_ScopedToFilePath pins that the file_path filter
// is exact — symbols in other files must NOT leak into the result.
func TestSymbolsInFile_ScopedToFilePath(t *testing.T) {
	c, err := Open(seedSymbolGraph(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	syms, _ := c.SymbolsInFile(context.Background(), "editor.tsx")
	for _, s := range syms {
		if s.Name == "unrelated" {
			t.Errorf("symbol from other.go leaked: %v", s)
		}
	}
}

// TestSymbolsInFile_UnavailableClientReturnsEmpty pins the
// schema-tolerance contract: an unavailable client (no DB, missing
// file) returns nil + nil — never an error. Mirrors the existing
// FunctionsInFile / ImportsInFile / CallersOf behaviour.
func TestSymbolsInFile_UnavailableClientReturnsEmpty(t *testing.T) {
	c := NewMissing()
	syms, err := c.SymbolsInFile(context.Background(), "editor.tsx")
	if err != nil {
		t.Errorf("unavailable client returned error: %v", err)
	}
	if syms != nil {
		t.Errorf("unavailable client returned non-nil: %v", syms)
	}
}

// TestSymbolsInFile_TolerantOfMissingTable pins that an existing
// graph.db WITHOUT a `nodes` table doesn't crash — returns an empty
// slice silently, matching the FunctionsInFile pattern.
func TestSymbolsInFile_TolerantOfMissingTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE placeholder (x INT)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	c, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syms, err := c.SymbolsInFile(context.Background(), "anything.go")
	if err != nil {
		t.Errorf("schema-missing returned error: %v", err)
	}
	if syms != nil {
		t.Errorf("schema-missing returned non-nil: %v", syms)
	}
}

// TestStale_FileNewerThanDB_ReportsStale pins V7-13 Gap 3: when the
// source file has been edited after codegraph last indexed it,
// Stale() returns true so callers skip the symbol pre-fetch.
func TestStale_FileNewerThanDB_ReportsStale(t *testing.T) {
	dbPath := seedSymbolGraph(t)
	// Backdate the DB so the file we create after it looks newer.
	dbMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(dbPath, dbMtime, dbMtime); err != nil {
		t.Fatalf("chtimes db: %v", err)
	}
	// Create a fresh file that's clearly newer than the DB.
	filePath := filepath.Join(filepath.Dir(dbPath), "freshly-edited.tsx")
	if err := os.WriteFile(filePath, []byte("export const x = 1\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	c, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	if !c.Stale(filePath) {
		t.Errorf("Stale() should return true: file is 1h newer than DB")
	}
}

// TestStale_DBNewerThanFile_ReportsFresh pins the happy path: when
// the codegraph index has been rebuilt after the file's last edit,
// Stale() returns false and the symbol pre-fetch proceeds.
func TestStale_DBNewerThanFile_ReportsFresh(t *testing.T) {
	dbPath := seedSymbolGraph(t)
	filePath := filepath.Join(filepath.Dir(dbPath), "stable.tsx")
	if err := os.WriteFile(filePath, []byte("export const x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate the file so the DB looks newer.
	fileMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(filePath, fileMtime, fileMtime); err != nil {
		t.Fatal(err)
	}
	c, _ := Open(dbPath)
	defer c.Close()
	if c.Stale(filePath) {
		t.Errorf("Stale() should return false: DB is newer than file")
	}
}

// TestStale_WithinSlack_ReportsFresh pins that small mtime jitter
// (file mtime ≤ DB mtime + slack) does NOT mark the file stale. The
// indexer's natural latency between save + index would otherwise
// cause spurious skip events.
func TestStale_WithinSlack_ReportsFresh(t *testing.T) {
	dbPath := seedSymbolGraph(t)
	filePath := filepath.Join(filepath.Dir(dbPath), "near.tsx")
	if err := os.WriteFile(filePath, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// File mtime = DB mtime + 2 seconds (well under the 5s slack).
	dbInfo, _ := os.Stat(dbPath)
	near := dbInfo.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(filePath, near, near); err != nil {
		t.Fatal(err)
	}
	c, _ := Open(dbPath)
	defer c.Close()
	if c.Stale(filePath) {
		t.Errorf("Stale() should fail-fresh inside the 5s slack window")
	}
}

// TestStale_UnavailableClientReportsFresh pins the fail-open
// semantic — an unavailable client never reports stale (callers
// should already gate on Available() before consulting Stale, but
// the fail-open contract makes mis-orderings safe).
func TestStale_UnavailableClientReportsFresh(t *testing.T) {
	if NewMissing().Stale("/tmp/anywhere") {
		t.Errorf("unavailable client must report not stale")
	}
}

// seedSymbolGraphWithCalls extends seedSymbolGraph with a callers/
// callees CALLS edge graph between symbols 2 (handleClick), 3 (render)
// and 1 (Editor). Returns the dbPath. Used by the V7-12 / get_symbols
// relation tests.
//
// Edge layout:
//
//	render (3) → handleClick (2)        — render calls handleClick
//	render (3) → Editor (1)              — render calls Editor (instantiates)
//	Editor (1) → handleClick (2)         — Editor also calls handleClick
//	handleClick (2) → render (3)         — handleClick calls render (re-render)
//
// So for symbol 2 (handleClick):
//
//	callers = [render(3), Editor(1)]
//	callees = [render(3)]
//	callers_count = 2; callees_count = 1
func seedSymbolGraphWithCalls(t *testing.T) string {
	t.Helper()
	dbPath := seedSymbolGraph(t)
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stmts := []string{
		`CREATE TABLE edges (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			kind TEXT NOT NULL
		)`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (3, 2, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (3, 1, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 2, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (2, 3, 'CALLS')`,
	}
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return dbPath
}

// TestFindSymbols_DiscoveryMode pins V7-12 discovery semantics: when
// neither name nor fqn is supplied, return every user-facing symbol
// in the file (functions, methods, classes, interfaces, types) in
// start_line ASC order. Variables and parameters are excluded.
func TestFindSymbols_DiscoveryMode(t *testing.T) {
	c, _ := Open(seedSymbolGraph(t))
	defer c.Close()

	got, err := c.FindSymbols(context.Background(), "editor.tsx", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"EditorProps", "EditorState", "handleClick", "Editor", "render"}
	if len(got) != len(wantOrder) {
		t.Fatalf("discovery: got %d, want %d (%v)", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if got[i].Name != want {
			t.Errorf("discovery[%d]: got %q want %q", i, got[i].Name, want)
		}
	}
	// Spot-check metadata is populated end-to-end (fqn, language,
	// ID present).
	if got[0].Language != "typescript" || got[0].FQN == "" || got[0].ID == 0 {
		t.Errorf("metadata not populated: %+v", got[0])
	}
}

// TestFindSymbols_NameFilter pins name-only lookup: returns every
// symbol with that name regardless of kind/fqn so V7-15 ranking can
// disambiguate.
func TestFindSymbols_NameFilter(t *testing.T) {
	// Add a second handleClick to test ambiguity surfacing.
	dbPath := seedSymbolGraph(t)
	d, _ := sql.Open("sqlite", "file:"+dbPath)
	if _, err := d.Exec(
		`INSERT INTO nodes VALUES (9, 'method', 'handleClick', 'editor.tsx', 220, 230, 'Editor.handleClick', 'typescript')`,
	); err != nil {
		t.Fatal(err)
	}
	d.Close()

	c, _ := Open(dbPath)
	defer c.Close()
	got, err := c.FindSymbols(context.Background(), "editor.tsx", "handleClick", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 handleClick matches, got %d: %v", len(got), got)
	}
	// Order is start_line ASC: 50 (function), 220 (method).
	if got[0].StartLine != 50 || got[1].StartLine != 220 {
		t.Errorf("start_line order wrong: %d, %d", got[0].StartLine, got[1].StartLine)
	}
}

// TestFindSymbols_FQNFilter pins exact-fqn disambiguation: when the
// agent supplies fqn, no other match should come back even if the
// shared `name` matches multiple symbols.
func TestFindSymbols_FQNFilter(t *testing.T) {
	dbPath := seedSymbolGraph(t)
	d, _ := sql.Open("sqlite", "file:"+dbPath)
	_, _ = d.Exec(`INSERT INTO nodes VALUES (9, 'method', 'handleClick', 'editor.tsx', 220, 230, 'Editor.handleClick', 'typescript')`)
	d.Close()

	c, _ := Open(dbPath)
	defer c.Close()
	got, err := c.FindSymbols(context.Background(), "editor.tsx", "", "Editor.handleClick", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FQN != "Editor.handleClick" {
		t.Fatalf("expected single Editor.handleClick match, got %v", got)
	}
}

// TestFindSymbols_KindFilter pins ANDed kind filter.
func TestFindSymbols_KindFilter(t *testing.T) {
	c, _ := Open(seedSymbolGraph(t))
	defer c.Close()
	got, _ := c.FindSymbols(context.Background(), "editor.tsx", "", "", "class")
	if len(got) != 1 || got[0].Name != "Editor" {
		t.Errorf("class filter: got %v", got)
	}
}

// TestFindSymbols_MissingFileReturnsEmpty pins the empty-result
// (not error) contract for unknown file paths.
func TestFindSymbols_MissingFileReturnsEmpty(t *testing.T) {
	c, _ := Open(seedSymbolGraph(t))
	defer c.Close()
	got, err := c.FindSymbols(context.Background(), "no-such.ts", "", "", "")
	if err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing file: got %v, want nil", got)
	}
}

// TestFindSymbols_UnavailableClientReturnsEmpty pins fail-open.
func TestFindSymbols_UnavailableClientReturnsEmpty(t *testing.T) {
	got, err := NewMissing().FindSymbols(context.Background(), "any.ts", "", "", "")
	if err != nil || got != nil {
		t.Errorf("unavailable client: got (%v, %v), want (nil, nil)", got, err)
	}
}

// TestCallersOfSymbol_RoundTrip pins the CALLS edge JOIN + metadata
// enrichment. Per the seed, handleClick (id 2) is called by render
// (id 3, line 200) and Editor (id 1, line 120). Sort by start_line
// ASC means Editor comes before render.
func TestCallersOfSymbol_RoundTrip(t *testing.T) {
	c, _ := Open(seedSymbolGraphWithCalls(t))
	defer c.Close()
	got, err := c.CallersOfSymbol(context.Background(), 2, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 callers, got %d: %v", len(got), got)
	}
	if got[0].Name != "Editor" || got[1].Name != "render" {
		t.Errorf("sort: got [%s, %s], want [Editor, render]", got[0].Name, got[1].Name)
	}
	// Spot-check metadata.
	if got[0].StartLine != 120 || got[0].FQN != "Editor" || got[0].Kind != "class" {
		t.Errorf("Editor caller metadata: %+v", got[0])
	}
}

// TestCalleesOfSymbol_RoundTrip pins the inverse direction.
// handleClick (id 2) calls render (id 3).
func TestCalleesOfSymbol_RoundTrip(t *testing.T) {
	c, _ := Open(seedSymbolGraphWithCalls(t))
	defer c.Close()
	got, err := c.CalleesOfSymbol(context.Background(), 2, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "render" {
		t.Errorf("callees: got %v", got)
	}
}

// TestCallersOfSymbol_RespectsLimit pins the LIMIT — even when the
// underlying graph has more, the slice is capped.
func TestCallersOfSymbol_RespectsLimit(t *testing.T) {
	c, _ := Open(seedSymbolGraphWithCalls(t))
	defer c.Close()
	got, err := c.CallersOfSymbol(context.Background(), 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("limit=1: got %d rows", len(got))
	}
}

// TestCallersOfSymbol_ZeroSymbolIDReturnsEmpty pins the guard.
func TestCallersOfSymbol_ZeroSymbolIDReturnsEmpty(t *testing.T) {
	c, _ := Open(seedSymbolGraphWithCalls(t))
	defer c.Close()
	got, _ := c.CallersOfSymbol(context.Background(), 0, 20)
	if got != nil {
		t.Errorf("symbolID=0 should return nil, got %v", got)
	}
}

// TestCountCallers_RoundTrip pins the unlimited-count contract.
// handleClick (id 2) has 2 callers; CountCallers reports 2 even
// though CallersOfSymbol caps at LIMIT.
func TestCountCallers_RoundTrip(t *testing.T) {
	c, _ := Open(seedSymbolGraphWithCalls(t))
	defer c.Close()
	n, err := c.CountCallers(context.Background(), 2)
	if err != nil || n != 2 {
		t.Errorf("CountCallers(handleClick): got (%d, %v), want (2, nil)", n, err)
	}
}

// TestCountCallees_RoundTrip pins the inverse direction.
func TestCountCallees_RoundTrip(t *testing.T) {
	c, _ := Open(seedSymbolGraphWithCalls(t))
	defer c.Close()
	n, err := c.CountCallees(context.Background(), 2)
	if err != nil || n != 1 {
		t.Errorf("CountCallees(handleClick): got (%d, %v), want (1, nil)", n, err)
	}
}

// TestCountCallers_UnavailableClientReturnsZero pins fail-open.
func TestCountCallers_UnavailableClientReturnsZero(t *testing.T) {
	n, err := NewMissing().CountCallers(context.Background(), 1)
	if n != 0 || err != nil {
		t.Errorf("unavailable: got (%d, %v)", n, err)
	}
}

// seedGraphWithCallsCycle builds a small CALLS graph for BFS testing.
// Layout (CALLS edges, source → target):
//
//	A (id 1) → B (id 2)
//	B (id 2) → C (id 3)
//	C (id 3) → A (id 1)   // cycle back to A
//	A (id 1) → D (id 4)
//
// So from A: callees at depth 1 = {B, D}; depth 2 = {B, C, D};
// depth 3 = {A, B, C, D} (A reachable via cycle).
//
// All nodes live in file "graph.ts".
func seedGraphWithCallsCycle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	d, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stmts := []string{
		`CREATE TABLE nodes (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			file_path TEXT,
			start_line INTEGER,
			end_line INTEGER,
			fqn TEXT,
			language TEXT
		)`,
		`CREATE TABLE edges (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			kind TEXT NOT NULL
		)`,
		`INSERT INTO nodes VALUES (1, 'function', 'A', 'graph.ts', 10, 20, 'A', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'function', 'B', 'graph.ts', 30, 40, 'B', 'typescript')`,
		`INSERT INTO nodes VALUES (3, 'function', 'C', 'graph.ts', 50, 60, 'C', 'typescript')`,
		`INSERT INTO nodes VALUES (4, 'function', 'D', 'graph.ts', 70, 80, 'D', 'typescript')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 2, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (2, 3, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (3, 1, 'CALLS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 4, 'CALLS')`,
	}
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return dbPath
}

// TestReachable_CalleesOneHop pins the simple BFS case: depth=1
// returns exactly the direct callees of the anchor.
func TestReachable_CalleesOneHop(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	got, trunc, err := c.Reachable(context.Background(), 1, RelationCallees, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Errorf("unexpected truncation: %v", got)
	}
	wantNames := []string{"B", "D"} // start_line ASC: B@30, D@70
	if len(got) != 2 || got[0].Name != wantNames[0] || got[1].Name != wantNames[1] {
		t.Errorf("got %+v, want names %v", got, wantNames)
	}
	for _, r := range got {
		if r.Depth != 1 || r.ViaEdge != "CALLS" {
			t.Errorf("depth/edge: %+v", r)
		}
	}
}

// TestReachable_CalleesTwoHops pins BFS to depth 2: walks one more
// hop from each depth-1 neighbor.
func TestReachable_CalleesTwoHops(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	got, _, _ := c.Reachable(context.Background(), 1, RelationCallees, 2, 100)
	// From A: depth 1 = {B, D}; depth 2 adds {C} (B→C). A appears at
	// depth 3 via the cycle, which is outside the depth=2 bound.
	wantNames := map[string]bool{"B": true, "D": true, "C": true}
	if len(got) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if !wantNames[r.Name] {
			t.Errorf("unexpected name %q", r.Name)
		}
	}
}

// TestReachable_CycleSafe pins that a cycle doesn't blow up the
// query — depth=3 should pick up A via the cycle but stop there.
func TestReachable_CycleSafe(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	got, _, err := c.Reachable(context.Background(), 1, RelationCallees, 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	// All four nodes are reachable: B, D (1), C (2), A (3 via cycle).
	// Should NOT exceed 4 even though depth=5 is requested.
	if len(got) != 4 {
		t.Errorf("cycle blew up or under-collected: got %d, want 4: %+v", len(got), got)
	}
}

// TestReachable_CallersDirection pins the reverse CALLS direction.
// From A: callers = {C} (depth 1 — C→A is the only inbound edge);
// depth 2 = {C, B} (B→C→A).
func TestReachable_CallersDirection(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	got, _, _ := c.Reachable(context.Background(), 1, RelationCallers, 1, 100)
	if len(got) != 1 || got[0].Name != "C" {
		t.Errorf("callers depth=1: got %+v, want [C]", got)
	}
	got2, _, _ := c.Reachable(context.Background(), 1, RelationCallers, 2, 100)
	names := map[string]bool{}
	for _, r := range got2 {
		names[r.Name] = true
	}
	if !names["C"] || !names["B"] {
		t.Errorf("callers depth=2: got %+v, want B and C present", got2)
	}
}

// TestReachable_TruncationFlag pins that hitting maxResults sets
// the truncated flag. Use the cycle graph with max_results=2.
func TestReachable_TruncationFlag(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	got, trunc, _ := c.Reachable(context.Background(), 1, RelationCallees, 5, 2)
	if len(got) != 2 {
		t.Errorf("expected 2 results at LIMIT 2, got %d", len(got))
	}
	if !trunc {
		t.Errorf("expected truncated=true at LIMIT 2")
	}
}

// TestReachable_ContainsBFS pins CONTAINS edge traversal when the
// kind IS populated. Seeds a parent→child graph explicitly.
func TestReachable_ContainsBFS(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	d, _ := sql.Open("sqlite", "file:"+dbPath)
	stmts := []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, kind TEXT, name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, fqn TEXT, language TEXT)`,
		`CREATE TABLE edges (id INTEGER PRIMARY KEY, source_id INTEGER, target_id INTEGER, kind TEXT)`,
		`INSERT INTO nodes VALUES (1, 'module', 'pkg', 'mod.ts', 1, 100, 'pkg', 'typescript')`,
		`INSERT INTO nodes VALUES (2, 'class', 'Foo', 'mod.ts', 10, 50, 'pkg.Foo', 'typescript')`,
		`INSERT INTO nodes VALUES (3, 'method', 'bar', 'mod.ts', 20, 30, 'pkg.Foo.bar', 'typescript')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (1, 2, 'CONTAINS')`,
		`INSERT INTO edges (source_id, target_id, kind) VALUES (2, 3, 'CONTAINS')`,
	}
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	c, _ := Open(dbPath)
	defer c.Close()
	got, _, _ := c.Reachable(context.Background(), 1, RelationContains, 5, 100)
	if len(got) != 2 {
		t.Fatalf("contains BFS: got %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "Foo" || got[0].Depth != 1 ||
		got[1].Name != "bar" || got[1].Depth != 2 {
		t.Errorf("contains BFS order/depth wrong: %+v", got)
	}
	for _, r := range got {
		if r.ViaEdge != "CONTAINS" {
			t.Errorf("expected via_edge=CONTAINS, got %q", r.ViaEdge)
		}
	}
}

// TestReachable_UnavailableClientReturnsEmpty pins fail-open.
func TestReachable_UnavailableClientReturnsEmpty(t *testing.T) {
	got, trunc, err := NewMissing().Reachable(context.Background(), 1, RelationCallees, 5, 100)
	if got != nil || trunc || err != nil {
		t.Errorf("unavailable: got (%v, %v, %v)", got, trunc, err)
	}
}

// TestReachable_ZeroAnchorIDReturnsEmpty pins the guard.
func TestReachable_ZeroAnchorIDReturnsEmpty(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	got, _, _ := c.Reachable(context.Background(), 0, RelationCallees, 5, 100)
	if got != nil {
		t.Errorf("anchorID=0 should return nil, got %+v", got)
	}
}

// TestCountEdgesByKind_RoundTrip pins the CALLS count helper used by
// get_relations' "CONTAINS not populated" hint detection.
func TestCountEdgesByKind_RoundTrip(t *testing.T) {
	c, _ := Open(seedGraphWithCallsCycle(t))
	defer c.Close()
	n, err := c.CountEdgesByKind(context.Background(), "CALLS")
	if err != nil || n != 4 {
		t.Errorf("CountEdgesByKind(CALLS): got (%d, %v), want (4, nil)", n, err)
	}
	// CONTAINS not seeded → 0.
	zero, err := c.CountEdgesByKind(context.Background(), "CONTAINS")
	if err != nil || zero != 0 {
		t.Errorf("CountEdgesByKind(CONTAINS): got (%d, %v), want (0, nil)", zero, err)
	}
}

// TestCountEdgesByKind_UnavailableReturnsZero pins fail-open.
func TestCountEdgesByKind_UnavailableReturnsZero(t *testing.T) {
	n, err := NewMissing().CountEdgesByKind(context.Background(), "CALLS")
	if n != 0 || err != nil {
		t.Errorf("unavailable: got (%d, %v)", n, err)
	}
}

// captureSlog redirects slog.Default to a buffer for the test's
// duration. Used by the V7-13 Gap 5 (b) re-index warning tests below.
// NOT safe for t.Parallel — slog.Default is global state.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	return &buf
}

// TestReindexWarning_FiresOnceOnMTimeBump pins V7-13 Gap 5 (b):
// when the codegraph DB's mtime changes after Open, the FIRST query
// that hits the boundary logs ONE warning. Subsequent queries don't
// re-log (atomic CAS gate).
func TestReindexWarning_FiresOnceOnMTimeBump(t *testing.T) {
	logs := captureSlog(t)
	path := seedGraphWithCallsCycle(t)
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	// First query — mtime unchanged, no warning expected.
	_, _ = c.CountEdgesByKind(context.Background(), "CALLS")
	if strings.Contains(logs.String(), "index modified since startup") {
		t.Errorf("warning fired before mtime bumped:\n%s", logs.String())
	}

	// Bump the DB file's mtime to simulate codebase-memory-mcp's
	// indexer re-writing the graph between observer startup and a
	// subsequent query. Use a Δ that exceeds filesystem mtime
	// resolution (1s on ext4 default).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Second query — mtime now differs from startupMTime, warning fires.
	_, _ = c.CountEdgesByKind(context.Background(), "CALLS")
	first := logs.String()
	if !strings.Contains(first, "index modified since startup") {
		t.Errorf("expected warning after mtime bump:\n%s", first)
	}

	// Third query — already warned; CAS gate prevents a second emit.
	logs.Reset()
	_, _ = c.CountEdgesByKind(context.Background(), "CALLS")
	if strings.Contains(logs.String(), "index modified since startup") {
		t.Errorf("warning fired a second time (CAS gate failed):\n%s", logs.String())
	}
}

// TestReindexWarning_NoFireWhenMTimeUnchanged: steady state — no
// warning when the DB hasn't been touched.
func TestReindexWarning_NoFireWhenMTimeUnchanged(t *testing.T) {
	logs := captureSlog(t)
	c, err := Open(seedGraphWithCallsCycle(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 5; i++ {
		_, _ = c.CountEdgesByKind(context.Background(), "CALLS")
	}
	if strings.Contains(logs.String(), "index modified since startup") {
		t.Errorf("unexpected warning in steady state:\n%s", logs.String())
	}
}

// TestReindexWarning_NoFireOnMissingDB: NewMissing client has no
// path / no startupMTime; checkReindex is a no-op. Confirms the
// warning machinery doesn't panic or log spuriously on the
// no-codegraph-installed case.
func TestReindexWarning_NoFireOnMissingDB(t *testing.T) {
	logs := captureSlog(t)
	c := NewMissing()
	for i := 0; i < 5; i++ {
		_, _ = c.CountEdgesByKind(context.Background(), "CALLS")
		_, _ = c.FunctionsInFile(context.Background(), "x.go")
	}
	if logs.Len() > 0 {
		t.Errorf("unavailable client logged unexpectedly:\n%s", logs.String())
	}
}
