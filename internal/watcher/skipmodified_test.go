package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// setupSkipModified builds a watcher against a fresh temp DB + watch root,
// wired identically to setup() in watcher_test.go except it also accepts
// Options.SkipModifiedBefore, which setup() doesn't expose. Model/harness
// (claude-code adapter, fixture reuse, temp DB) intentionally mirrors
// setup() so this file doesn't invent a second test convention.
func setupSkipModified(t *testing.T, skipBefore time.Time) (*Watcher, *store.Store, string) {
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
		Debounce:           50 * time.Millisecond,
		SkipModifiedBefore: skipBefore,
	})
	return w, s, watchRoot
}

// TestScanSkipModifiedBefore pins WP2's acceptance criteria: a non-zero
// SkipModifiedBefore skips a session file whose mtime predates the cutoff,
// still processes an in-window file, and the zero value changes nothing
// (both files processed, matching pre-existing behavior).
func TestScanSkipModifiedBefore(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claudecode", "simple-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The full fixture yields 4 actions per file (see
	// TestScanIngestsFixtureFile in watcher_test.go): user_prompt +
	// assistant_text + Read + Bash. Two distinct source_files with the
	// same body insert independently — the (source_file,
	// source_event_id) UNIQUE index is keyed per file, not per session.
	const actionsPerFile = 4

	cutoff := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	before := cutoff.Add(-48 * time.Hour)
	after := cutoff.Add(48 * time.Hour)

	tests := []struct {
		name           string
		skipBefore     time.Time
		wantProcessed  int
		wantActions    int
		preFileSkipped bool
	}{
		{
			name:           "skips pre-window file, processes in-window file",
			skipBefore:     cutoff,
			wantProcessed:  1,
			wantActions:    actionsPerFile,
			preFileSkipped: true,
		},
		{
			name:           "zero value processes both regardless of mtime",
			skipBefore:     time.Time{},
			wantProcessed:  2,
			wantActions:    2 * actionsPerFile,
			preFileSkipped: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, s, root := setupSkipModified(t, tt.skipBefore)

			preFile := writeJSONL(t, root, "pre-window.jsonl", fixture)
			postFile := writeJSONL(t, root, "in-window.jsonl", fixture)

			if err := os.Chtimes(preFile, before, before); err != nil {
				t.Fatalf("Chtimes pre-window: %v", err)
			}
			if err := os.Chtimes(postFile, after, after); err != nil {
				t.Fatalf("Chtimes in-window: %v", err)
			}

			ctx := context.Background()
			res, err := w.Scan(ctx)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if res.FilesProcessed != tt.wantProcessed {
				t.Errorf("FilesProcessed = %d, want %d", res.FilesProcessed, tt.wantProcessed)
			}
			if res.Errors != 0 {
				t.Errorf("Errors = %d, want 0", res.Errors)
			}
			n, err := s.CountActions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.wantActions {
				t.Errorf("actions after scan = %d, want %d", n, tt.wantActions)
			}

			// Cursor presence is a proxy for "was this file opened at
			// all" — a skipped file must never acquire a cursor row.
			preOff, preErr := s.GetCursor(ctx, preFile)
			if tt.preFileSkipped {
				if preErr != nil {
					t.Fatalf("GetCursor(pre-window): %v", preErr)
				}
				if preOff != 0 {
					t.Errorf("skipped pre-window file has cursor offset %d, want 0 (never opened)", preOff)
				}
			}
		})
	}
}

// TestScanSkipModifiedBeforeFollowsSymlinkTargetMtime pins F7: the scan's
// SkipModifiedBefore gate must derive a symlink's mtime from its TARGET,
// not the link's own (Lstat) info — because processFile follows safe
// symlinks and parses the target's content, not the link's. Backdating a
// symlink's own Lstat mtime independent of its target isn't portable
// across platforms via the standard library, so this pins the contract
// via the target's mtime instead, in a shape that is a real regression
// test: the symlink itself is always created "now" (well after the
// cutoff below), so the pre-fix code — which read the link's own
// Info() — would treat a stale-target symlink as in-window and process
// it. The fix must skip it instead, deciding on the target's mtime.
func TestScanSkipModifiedBeforeFollowsSymlinkTargetMtime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privilege on Windows")
	}

	cutoff := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	before := cutoff.Add(-48 * time.Hour)
	after := cutoff.Add(48 * time.Hour)

	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claudecode", "simple-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const actionsPerFile = 4

	tests := []struct {
		name          string
		targetMtime   time.Time
		wantProcessed bool
	}{
		{"target predates cutoff via the symlink path: skipped", before, false},
		{"target in-window via the symlink path: processed", after, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, s, root := setupSkipModified(t, cutoff)

			// The target uses a non-.jsonl extension so the walk's own
			// IsSessionFile(path) check excludes IT directly as a walk
			// entry — only the symlink (named with the .jsonl suffix
			// IsSessionFile requires) is a scan candidate here.
			target := filepath.Join(root, "real-session.data")
			if err := os.WriteFile(target, fixture, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(target, tt.targetMtime, tt.targetMtime); err != nil {
				t.Fatalf("Chtimes target: %v", err)
			}

			link := filepath.Join(root, "session-link.jsonl")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			res, err := w.Scan(ctx)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			wantProcessed, wantActions := 0, 0
			if tt.wantProcessed {
				wantProcessed, wantActions = 1, actionsPerFile
			}
			if res.FilesProcessed != wantProcessed {
				t.Errorf("FilesProcessed = %d, want %d", res.FilesProcessed, wantProcessed)
			}
			if res.Errors != 0 {
				t.Errorf("Errors = %d, want 0", res.Errors)
			}
			n, err := s.CountActions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != wantActions {
				t.Errorf("actions after scan = %d, want %d", n, wantActions)
			}

			off, cursorErr := s.GetCursor(ctx, link)
			if cursorErr != nil {
				t.Fatalf("GetCursor(link): %v", cursorErr)
			}
			if tt.wantProcessed && off == 0 {
				t.Errorf("processed symlink has zero cursor offset, want > 0 (was opened)")
			}
			if !tt.wantProcessed && off != 0 {
				t.Errorf("skipped symlink has cursor offset %d, want 0 (never opened)", off)
			}
		})
	}
}
