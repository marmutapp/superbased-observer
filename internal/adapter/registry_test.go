package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

type fakeAdapter struct {
	name  string
	paths []string
}

func (f *fakeAdapter) Name() string              { return f.name }
func (f *fakeAdapter) WatchPaths() []string      { return f.paths }
func (f *fakeAdapter) IsSessionFile(string) bool { return true }
func (f *fakeAdapter) ParseSessionFile(context.Context, string, int64) (ParseResult, error) {
	return ParseResult{}, nil
}

func TestRegistryRegisterAndGet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	a := &fakeAdapter{name: models.ToolClaudeCode}
	r.Register(a)
	if r.Get(models.ToolClaudeCode) != a {
		t.Fatal("Get returned wrong adapter")
	}
	if r.Get("unknown") != nil {
		t.Fatal("Get for unknown name should return nil")
	}
}

func TestRegistryAllSortedByName(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(&fakeAdapter{name: "b"})
	r.Register(&fakeAdapter{name: "a"})
	r.Register(&fakeAdapter{name: "c"})
	got := r.All()
	if len(got) != 3 || got[0].Name() != "a" || got[1].Name() != "b" || got[2].Name() != "c" {
		t.Errorf("All not sorted: %v", names(got))
	}
}

func TestDetectedChecksWatchPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existing := filepath.Join(dir, "claude")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.Register(&fakeAdapter{name: models.ToolClaudeCode, paths: []string{existing}})
	r.Register(&fakeAdapter{name: models.ToolCodex, paths: []string{filepath.Join(dir, "codex-does-not-exist")}})

	got := r.Detected(nil)
	if len(got) != 1 || got[0].Name() != models.ToolClaudeCode {
		t.Errorf("Detected: got %v", names(got))
	}
}

func TestDetectedRespectsAllowList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.Register(&fakeAdapter{name: models.ToolClaudeCode, paths: []string{dir}})
	r.Register(&fakeAdapter{name: models.ToolCodex, paths: []string{dir}})

	got := r.Detected([]string{models.ToolCodex})
	if len(got) != 1 || got[0].Name() != models.ToolCodex {
		t.Errorf("allow list not honored: %v", names(got))
	}
}

// TestDetectedExplicitEmptyAllowDisablesAllAdapters pins the
// nil-vs-empty distinction. A user who writes
// `enabled_adapters = []` in their config.toml expects "no adapters"
// — pre-fix that came back as "all adapters" (silent footgun that
// polluted the A/B observer DBs in /tmp/ab-claude). nil still means
// "no filter" so callers using watcher.Options{} (zero-value) keep
// getting the full set.
func TestDetectedExplicitEmptyAllowDisablesAllAdapters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	r.Register(&fakeAdapter{name: models.ToolClaudeCode, paths: []string{dir}})
	r.Register(&fakeAdapter{name: models.ToolCodex, paths: []string{dir}})

	// Explicit empty (non-nil) → zero adapters.
	if got := r.Detected([]string{}); len(got) != 0 {
		t.Errorf("explicit-empty allow returned %d adapters, want 0: %v", len(got), names(got))
	}
	// Nil → no filter, all detected.
	if got := r.Detected(nil); len(got) != 2 {
		t.Errorf("nil allow returned %d adapters, want 2: %v", len(got), names(got))
	}
}

func names(as []Adapter) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name()
	}
	return out
}
