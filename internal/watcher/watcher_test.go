package watcher

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func setup(t *testing.T) (*Watcher, *store.Store, string) {
	t.Helper()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "w.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := store.New(database)

	watchRoot := t.TempDir()
	reg := adapter.NewRegistry()
	reg.Register(claudecode.NewWithOptions(nil, watchRoot))

	w := New(s, reg, Options{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		NativePredicate: map[string]func(string) bool{
			"claude-code": claudecode.IsNativeTool,
		},
		Debounce: 50 * time.Millisecond,
	})
	return w, s, watchRoot
}

// writeJSONL copies one of the fixture JSONL files into dst under the watch
// root so the adapter can find it.
func writeJSONL(t *testing.T, watchRoot, name string, body []byte) string {
	t.Helper()
	dst := filepath.Join(watchRoot, name)
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestScanIngestsFixtureFile(t *testing.T) {
	t.Parallel()
	w, s, root := setup(t)
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claudecode", "simple-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, root, "session.jsonl", body)

	ctx := context.Background()
	res, err := w.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesProcessed != 1 {
		t.Errorf("files processed: %d", res.FilesProcessed)
	}
	n, _ := s.CountActions(ctx)
	// 3 actions: user_prompt (from line 1's user text) + Read + Bash.
	// Lines 3 and 5 are tool_result-only user messages and don't emit
	// user_prompt.
	if n != 3 {
		t.Errorf("actions after scan: %d want 3", n)
	}

	// Re-scan: cursor should prevent duplicates.
	res2, err := w.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n2, _ := s.CountActions(ctx)
	if n2 != 3 {
		t.Errorf("actions after re-scan: %d want 3", n2)
	}
	if res2.FilesProcessed != 1 {
		t.Errorf("re-scan files processed: %d", res2.FilesProcessed)
	}
}

func TestWatchPicksUpAppendedLines(t *testing.T) {
	t.Parallel()
	w, s, root := setup(t)

	// Seed with the first two lines of the fixture, so we have a valid
	// tool_use followed by a tool_result pair.
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claudecode", "simple-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Split into lines.
	var l1End int
	for i, b := range body {
		if b == '\n' {
			if l1End == 0 {
				l1End = i + 1
				continue
			}
			// Second newline: keep lines 1+2 initially.
			path := writeJSONL(t, root, "grow.jsonl", body[:i+1])
			_ = path
			break
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx) }()

	// Wait for the initial scan to finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.CountActions(ctx)
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n, _ := s.CountActions(ctx); n < 1 {
		t.Fatalf("initial scan did not ingest: %d", n)
	}

	// Append the rest.
	p := filepath.Join(root, "grow.jsonl")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.CountActions(ctx)
		if n >= 3 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	// After the full fixture lands: user_prompt (line 1) + Read (line 2)
	// + Bash (line 4) = 3 actions.
	if n, _ := s.CountActions(ctx); n != 3 {
		t.Errorf("watch did not pick up new lines: actions=%d want 3", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit")
	}
}

// TestPollCursorsCatchesUpDroppedWrites simulates the WSL2/NTFS
// fsnotify-drop scenario: a Write that the OS never delivers to
// fsnotify leaves parse_cursors behind on-disk file size. pollCursors
// is the safety net — it stat()s every known cursor and re-runs
// processFile when bytes are pending.
func TestPollCursorsCatchesUpDroppedWrites(t *testing.T) {
	t.Parallel()
	w, s, root := setup(t)
	full, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claudecode", "simple-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Split on newline so the partial file is still valid JSONL. Take
	// the first two lines (user_prompt + Read), leaving the Bash on
	// line 4 to be picked up by the poll pass.
	lines := bytes.SplitAfter(full, []byte("\n"))
	if len(lines) < 4 {
		t.Fatalf("fixture has only %d lines; expected >=4", len(lines))
	}
	prefix := bytes.Join(lines[:2], nil)
	path := writeJSONL(t, root, "session.jsonl", prefix)

	ctx := context.Background()
	if _, err := w.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	n0, _ := s.CountActions(ctx)
	if n0 != 2 {
		t.Fatalf("after partial scan: got %d actions, want 2", n0)
	}

	// Append the remainder out-of-band (no fsnotify event delivered).
	rest := bytes.Join(lines[2:], nil)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(rest); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// pollCursors should detect file_size > byte_offset and re-process.
	if err := w.pollCursors(ctx); err != nil {
		t.Fatalf("pollCursors: %v", err)
	}
	n1, _ := s.CountActions(ctx)
	if n1 != 3 {
		t.Errorf("after pollCursors: got %d actions, want 3 (poll didn't catch up)", n1)
	}

	// Second poll on a now-current cursor must be a no-op.
	if err := w.pollCursors(ctx); err != nil {
		t.Fatalf("pollCursors (idempotent): %v", err)
	}
	n2, _ := s.CountActions(ctx)
	if n2 != n1 {
		t.Errorf("idempotent poll inserted rows: %d → %d", n1, n2)
	}
}

func TestPollCursorsSkipsOrphanPaths(t *testing.T) {
	t.Parallel()
	w, s, _ := setup(t)
	ctx := context.Background()

	// Seed a cursor for a path no adapter recognises (mimics older
	// adapter versions whose IsSessionFile has since been tightened).
	// pollCursors must skip it — same exclusion the dashboard health
	// endpoint applies — so the watcher doesn't churn on rows the
	// recovery flow can't process anyway.
	orphan := filepath.Join(t.TempDir(), "unknown-tool.log")
	if err := os.WriteFile(orphan, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(ctx, orphan, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.pollCursors(ctx); err != nil {
		t.Fatalf("pollCursors: %v", err)
	}
	// Cursor must remain at 0 — we never invoked an adapter.
	off, _ := s.GetCursor(ctx, orphan)
	if off != 0 {
		t.Errorf("orphan cursor advanced: %d", off)
	}
}

func TestNewClampsNegativePollInterval(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "w.db")
	database, _ := db.Open(context.Background(), db.Options{Path: dbPath})
	defer database.Close()
	w := New(store.New(database), adapter.NewRegistry(), Options{
		PollInterval: -5 * time.Second,
	})
	if w.pollInterval != 0 {
		t.Errorf("negative PollInterval not clamped: %v", w.pollInterval)
	}
}

func TestScanWithNoAdaptersIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	database, err := db.Open(ctx, db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := store.New(database)
	reg := adapter.NewRegistry()
	w := New(s, reg, Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})

	res, err := w.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.FilesProcessed != 0 {
		t.Errorf("expected zero files, got %d", res.FilesProcessed)
	}
}
