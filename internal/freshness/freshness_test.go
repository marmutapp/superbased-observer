package freshness

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.db")
	d, err := db.Open(ctx, db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	// Seed a projects row so FK constraints on actions succeed.
	_, err = d.ExecContext(ctx,
		`INSERT INTO projects (root_path, created_at) VALUES ('/tmp/p', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at)
		 VALUES ('s1', 1, ?, ?), ('s2', 1, ?, ?)`,
		models.ToolClaudeCode, time.Now().UTC().Format(time.RFC3339Nano),
		models.ToolClaudeCode, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func insertAction(t *testing.T, d *sql.DB, id int64, session, actionType, target string) {
	t.Helper()
	_, err := d.ExecContext(context.Background(),
		`INSERT INTO actions (
			id, session_id, project_id, timestamp, action_type, target, tool,
			source_file, source_event_id, success
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, 1)`,
		id, session, time.Now().UTC().Format(time.RFC3339Nano),
		actionType, target, models.ToolClaudeCode,
		target, "e-"+actionType+"-"+target,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyFirstAccessIsFresh(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10, FastPathStatOnly: true})
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	writeFile(t, p, "package a")

	obs, err := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Freshness != models.FreshnessFresh {
		t.Errorf("got %q want fresh", obs.Freshness)
	}
	if obs.ContentHash == "" {
		t.Error("expected content hash")
	}
	if obs.FileSizeBytes == 0 {
		t.Error("expected file size")
	}
}

func TestClassifyReReadInSameSessionIsStale(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10, FastPathStatOnly: true})
	dir := t.TempDir()
	p := filepath.Join(dir, "b.go")
	writeFile(t, p, "package b")

	// First read → fresh, then register in file_state tied to a real action.
	insertAction(t, d, 1, "s1", models.ActionReadFile, p)
	obs1, _ := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err := c.UpsertFileState(context.Background(), 1, p, obs1, 1, models.ActionReadFile, "s1"); err != nil {
		t.Fatal(err)
	}

	// Second read in same session with unchanged file → stale.
	obs2, err := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	if obs2.Freshness != models.FreshnessStale {
		t.Errorf("got %q want stale", obs2.Freshness)
	}
}

func TestClassifyCrossSessionUnchangedIsFresh(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10, FastPathStatOnly: true})
	dir := t.TempDir()
	p := filepath.Join(dir, "c.go")
	writeFile(t, p, "package c")

	insertAction(t, d, 1, "s1", models.ActionReadFile, p)
	obs1, _ := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err := c.UpsertFileState(context.Background(), 1, p, obs1, 1, models.ActionReadFile, "s1"); err != nil {
		t.Fatal(err)
	}

	// New session — content unchanged should still be "fresh" (not stale)
	// because the prior was in a different session.
	obs2, err := c.Classify(context.Background(), 1, "s2", models.ActionReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	if obs2.Freshness != models.FreshnessFresh {
		t.Errorf("cross-session unchanged: got %q want fresh", obs2.Freshness)
	}
	if obs2.ChangeDetected {
		t.Error("ChangeDetected should be false for identical content")
	}
}

func TestClassifyChangedBySelf(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10, FastPathStatOnly: true})
	dir := t.TempDir()
	p := filepath.Join(dir, "d.go")
	writeFile(t, p, "package d")

	// Read (1), then AI edits (2), seed file_state from the read.
	insertAction(t, d, 1, "s1", models.ActionReadFile, p)
	obs1, _ := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	_ = c.UpsertFileState(context.Background(), 1, p, obs1, 1, models.ActionReadFile, "s1")

	insertAction(t, d, 2, "s1", models.ActionEditFile, p)
	// File changes on disk as the AI writes.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, p, "package d\n// edited")

	obs2, err := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	if obs2.Freshness != models.FreshnessChangedBySelf {
		t.Errorf("got %q want changed_by_self", obs2.Freshness)
	}
	if !obs2.ChangeDetected {
		t.Error("ChangeDetected should be true")
	}
}

func TestClassifyChangedExternally(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10, FastPathStatOnly: true})
	dir := t.TempDir()
	p := filepath.Join(dir, "e.go")
	writeFile(t, p, "package e")

	insertAction(t, d, 1, "s1", models.ActionReadFile, p)
	obs1, _ := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	_ = c.UpsertFileState(context.Background(), 1, p, obs1, 1, models.ActionReadFile, "s1")

	// No intervening AI edit — user modified the file externally.
	time.Sleep(10 * time.Millisecond)
	writeFile(t, p, "package e\n// by user")

	obs2, err := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	if obs2.Freshness != models.FreshnessChangedExternally {
		t.Errorf("got %q want changed_externally", obs2.Freshness)
	}
}

func TestClassifyMissingFileIsUnknown(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10})
	obs, err := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, "/tmp/does-not-exist-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Freshness != models.FreshnessUnknown {
		t.Errorf("got %q want unknown", obs.Freshness)
	}
}

func TestClassifyIgnoredByPattern(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{
		MaxHashSizeMB:  10,
		IgnorePatterns: []string{"node_modules/", "*.wasm"},
	})
	dir := t.TempDir()

	inNode := filepath.Join(dir, "node_modules", "foo", "a.js")
	_ = os.MkdirAll(filepath.Dir(inNode), 0o755)
	writeFile(t, inNode, "x")
	obs, _ := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, inNode)
	if obs.Freshness != models.FreshnessUnknown {
		t.Errorf("node_modules: got %q want unknown", obs.Freshness)
	}

	wasm := filepath.Join(dir, "app.wasm")
	writeFile(t, wasm, "x")
	obs, _ = c.Classify(context.Background(), 1, "s1", models.ActionReadFile, wasm)
	if obs.Freshness != models.FreshnessUnknown {
		t.Errorf("*.wasm: got %q want unknown", obs.Freshness)
	}
}

func TestFastPathStatOnly(t *testing.T) {
	t.Parallel()
	d := setupDB(t)
	c := New(d, Options{MaxHashSizeMB: 10, FastPathStatOnly: true})
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	writeFile(t, p, "package f")

	// Seed file_state with a known hash and matching mtime/size.
	fi, _ := os.Stat(p)
	_, _ = d.ExecContext(context.Background(),
		`INSERT INTO file_state (project_id, file_path, content_hash, file_mtime, file_size_bytes, last_action_type, last_seen_at)
		 VALUES (1, ?, 'deadbeef', ?, ?, ?, ?)`,
		p, fi.ModTime().UTC().Format(time.RFC3339Nano), fi.Size(),
		models.ActionReadFile, time.Now().UTC().Format(time.RFC3339Nano),
	)

	obs, err := c.Classify(context.Background(), 1, "s1", models.ActionReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	// Fast path should reuse the stored hash (which is intentionally wrong
	// but whose point is: we didn't re-hash).
	if obs.ContentHash != "deadbeef" {
		t.Errorf("fast path did not use stored hash: %q", obs.ContentHash)
	}
}
