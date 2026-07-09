package clinecli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestName pins the adapter's Tool name to the wire-stable value the
// store + dashboard key off. Bumping it later would require a
// migration; pinning here makes the rename visible in code review.
func TestName(t *testing.T) {
	t.Parallel()
	if got := New().Name(); got != models.ToolClineCLI {
		t.Errorf("Name() = %q; want %q", got, models.ToolClineCLI)
	}
	if models.ToolClineCLI != "cline-cli" {
		t.Errorf("ToolClineCLI = %q; want \"cline-cli\"", models.ToolClineCLI)
	}
}

// TestWatchPathsRespectsCLINE_DIR confirms that setting CLINE_DIR makes
// the adapter walk that path in addition to the per-home defaults.
// Mirrors HERMES_HOME's behaviour. Test only runs against fresh
// defaultRoots() because NewWithOptions overrides the env-derived
// candidates entirely.
func TestWatchPathsRespectsCLINE_DIR(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-cline-home")
	t.Setenv("CLINE_DIR", custom)
	a := New()
	got := a.WatchPaths()
	if len(got) == 0 {
		t.Fatal("WatchPaths(): empty; expected at least the CLINE_DIR entry")
	}
	found := false
	for _, p := range got {
		if filepath.Clean(p) == filepath.Clean(custom) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WatchPaths() = %v; CLINE_DIR=%q not present", got, custom)
	}
}

// TestWatchPathsDefaultIncludesPerHome confirms that with no
// CLINE_DIR override the adapter still walks per-home .cline
// directories — crossmount.AllHomes() always yields at least the
// running process's $HOME on Windows/Linux/macOS.
func TestWatchPathsDefaultIncludesPerHome(t *testing.T) {
	t.Setenv("CLINE_DIR", "")
	a := New()
	roots := a.WatchPaths()
	if len(roots) == 0 {
		t.Fatal("WatchPaths(): empty; expected at least one ~/.cline candidate")
	}
	for _, p := range roots {
		if !strings.HasSuffix(filepath.ToSlash(p), "/.cline") {
			t.Errorf("WatchPaths() entry %q; expected suffix /.cline", p)
		}
	}
}

// TestIsSessionFile pins the three dispatch families: SQLite trio,
// per-session messages.json, hooks.jsonl. Each must be under one of
// the adapter's watch roots AND inside the right `.cline/data/*`
// subtree.
func TestIsSessionFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := NewWithOptions(nil, root)

	dataDir := filepath.Join(root, ".cline", "data")
	cases := []struct {
		name string
		path string
		want bool
	}{
		// --- SQLite trio ---
		{
			name: "sessions_db_under_root",
			path: filepath.Join(dataDir, "db", "sessions.db"),
			want: true,
		},
		{
			name: "sessions_db_wal_under_root",
			path: filepath.Join(dataDir, "db", "sessions.db-wal"),
			want: true,
		},
		{
			name: "sessions_db_shm_under_root",
			path: filepath.Join(dataDir, "db", "sessions.db-shm"),
			want: true,
		},
		// --- Per-session messages JSON (now intentionally NOT a
		// separate dispatch target — walked inside scanStateDB. The
		// hermes precedent: a single-table SQLite trigger covers
		// the bi-storage write.) ---
		{
			name: "messages_json_does_not_match",
			path: filepath.Join(dataDir, "sessions", "1780701711502_0v8d2", "1780701711502_0v8d2.messages.json"),
			want: false,
		},
		// --- Hook log ---
		{
			name: "hooks_jsonl_under_root",
			path: filepath.Join(dataDir, "logs", "hooks.jsonl"),
			want: true,
		},
		// --- Rejected: under-watch but wrong subtree ---
		{
			name: "metadata_json_does_not_match",
			path: filepath.Join(dataDir, "sessions", "1780701711502_0v8d2", "1780701711502_0v8d2.json"),
			want: false,
		},
		{
			name: "cron_db_does_not_match",
			path: filepath.Join(dataDir, "db", "cron.db"),
			want: false,
		},
		{
			name: "teams_db_does_not_match",
			path: filepath.Join(dataDir, "db", "teams.db"),
			want: false,
		},
		{
			name: "providers_json_does_not_match",
			path: filepath.Join(dataDir, "settings", "providers.json"),
			want: false,
		},
		// --- Rejected: shape-correct but outside watch root (v1.4.51 invariant) ---
		{
			name: "sessions_db_outside_root",
			path: "/tmp/foreign/cline/data/db/sessions.db",
			want: false,
		},
		{
			name: "hooks_jsonl_outside_root",
			path: "/tmp/foreign/cline/data/logs/hooks.jsonl",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := a.IsSessionFile(tc.path)
			if got != tc.want {
				t.Errorf("IsSessionFile(%q) = %v; want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestParseSessionFileUnknownPath confirms that paths the dispatch
// doesn't recognise return a no-op (NewOffset passthrough, zero
// events, no error). Defends the dispatch's `default:` fallthrough
// against accidental future recasting.
func TestParseSessionFileUnknownPath(t *testing.T) {
	t.Parallel()
	a := New()
	res, err := a.ParseSessionFile(context.Background(), "/tmp/garbage.txt", 12345)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if res.NewOffset != 12345 {
		t.Errorf("NewOffset = %d; want 12345 (fromOffset passthrough)", res.NewOffset)
	}
	if len(res.ToolEvents) != 0 || len(res.TokenEvents) != 0 {
		t.Errorf("events should be empty; got %d tool / %d token", len(res.ToolEvents), len(res.TokenEvents))
	}
}

// TestParseSessionFileHooksJsonlMissingFile confirms the hooks.jsonl
// path returns an error (not a panic) when the file doesn't exist
// at the dispatched path. End-to-end success cases live in
// hook_test.go::TestParseHooksJSONL_Adapter_E2E.
func TestParseSessionFileHooksJsonlMissingFile(t *testing.T) {
	t.Parallel()
	a := New()
	_, err := a.ParseSessionFile(context.Background(), "/nonexistent/.cline/data/logs/hooks.jsonl", 0)
	if err == nil {
		t.Error("expected error for missing hooks.jsonl; got nil")
	}
}
