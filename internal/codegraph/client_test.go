package codegraph

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
