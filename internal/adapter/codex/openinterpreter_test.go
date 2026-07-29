package codex

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// oiFixtureDir returns the directory holding the live-captured Open
// Interpreter rollout fixture (testdata/openinterpreter/sessions/
// 2026/07/17/), and oiFixturePath the file itself.
func oiFixtureDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "testdata", "openinterpreter", "sessions", "2026", "07", "17")
}

func oiFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(oiFixtureDir(t), "rollout-2026-07-17T14-59-49-019f6f69-23a0-7e32-bdba-9a9fabc946be.jsonl")
}

// TestOpenInterpreterName pins the retagged tool identity.
func TestOpenInterpreterName(t *testing.T) {
	t.Parallel()
	a := NewOpenInterpreter()
	if got := a.Name(); got != models.ToolOpenInterpreter {
		t.Errorf("Name() = %q, want %q", got, models.ToolOpenInterpreter)
	}
	if got := a.Name(); got == models.ToolCodex {
		t.Errorf("Name() = %q, must NOT collapse to the base codex identity", got)
	}
}

// TestOpenInterpreterWatchPathsDefaultRoots confirms platform-default
// discovery expands to ".openinterpreter/sessions" (not ".codex/
// sessions") under every cross-mount-resolved $HOME.
func TestOpenInterpreterWatchPathsDefaultRoots(t *testing.T) {
	t.Parallel()
	a := NewOpenInterpreter()
	paths := a.WatchPaths()
	if len(paths) == 0 {
		t.Fatal("WatchPaths() returned no roots")
	}
	for _, p := range paths {
		if !strings.HasSuffix(p, filepath.Join(".openinterpreter", "sessions")) {
			t.Errorf("root %q does not end in .openinterpreter/sessions", p)
		}
		if strings.HasSuffix(p, filepath.Join(".codex", "sessions")) {
			t.Errorf("root %q leaked the codex path shape", p)
		}
	}
}

// TestOpenInterpreterWatchPathsHonorsInterpreterHome mirrors
// TestWatchPathsHonorsCodexHome for the variant's own env var.
func TestOpenInterpreterWatchPathsHonorsInterpreterHome(t *testing.T) {
	t.Setenv("INTERPRETER_HOME", "/custom/openinterpreter")
	a := NewOpenInterpreter()
	paths := a.WatchPaths()
	want := filepath.Join("/custom/openinterpreter", "sessions")
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("INTERPRETER_HOME not honored: %v", paths)
	}
}

// TestOpenInterpreterCoexistsWithCodex is the checklist §2.1(d) guard:
// asserts BOTH branches — a fresh codex.New() instance is completely
// unaffected by the existence of the Open Interpreter variant (no
// shared mutable state, no accidental retag), and IsSessionFile /
// WatchPaths stay scoped to each instance's own roots.
func TestOpenInterpreterCoexistsWithCodex(t *testing.T) {
	t.Parallel()
	c := New()
	oi := NewOpenInterpreter()

	if got := c.Name(); got != models.ToolCodex {
		t.Errorf("codex.New().Name() = %q, want %q (must not be retagged)", got, models.ToolCodex)
	}
	if got := oi.Name(); got != models.ToolOpenInterpreter {
		t.Errorf("NewOpenInterpreter().Name() = %q, want %q", got, models.ToolOpenInterpreter)
	}

	for _, p := range c.WatchPaths() {
		if strings.HasSuffix(p, filepath.Join(".openinterpreter", "sessions")) {
			t.Errorf("codex WatchPaths leaked an .openinterpreter root: %q", p)
		}
	}
	for _, p := range oi.WatchPaths() {
		if strings.HasSuffix(p, filepath.Join(".codex", "sessions")) {
			t.Errorf("open-interpreter WatchPaths leaked a .codex root: %q", p)
		}
	}

	// A codex rollout file under the OI instance's own watch root is
	// still recognized (identical filename shape) — only the root
	// discriminates which instance owns a given file in the real
	// watcher dispatch, not the adapter's IsSessionFile predicate
	// shape itself.
	dir := t.TempDir()
	oiScoped := NewOpenInterpreterWithOptions(nil, dir)
	if !oiScoped.IsSessionFile(filepath.Join(dir, "rollout-2026-04-16-abc.jsonl")) {
		t.Error("rollout-*.jsonl under the OI instance's own watch root should match")
	}
}

// TestOpenInterpreterParsesRealFixtureEndToEnd parses the live-
// captured rollout fixture and asserts: every emitted ToolEvent/
// TokenEvent is retagged models.ToolOpenInterpreter, the gross→net
// token math nets the cached portion out of input exactly as codex's
// shared parser already does, the model string round-trips
// (gpt-5.6-sol), and cwd/project-root resolution succeeds (this
// fixture's cwd happens to be this very repo's path, so ProjectRoot
// must be non-empty — see resolveProjectRoot's git.Resolve-or-
// fallback-to-cwd contract).
func TestOpenInterpreterParsesRealFixtureEndToEnd(t *testing.T) {
	t.Parallel()
	dir := oiFixtureDir(t)
	path := oiFixturePath(t)
	a := NewOpenInterpreterWithOptions(nil, dir)

	if !a.IsSessionFile(path) {
		t.Fatalf("IsSessionFile(%q) = false, want true", path)
	}

	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	if len(res.ToolEvents) == 0 {
		t.Fatal("no ToolEvents parsed from fixture")
	}
	if len(res.TokenEvents) != 2 {
		t.Fatalf("TokenEvents count = %d, want 2 (fixture has two token_count events)", len(res.TokenEvents))
	}

	for i, e := range res.ToolEvents {
		if e.Tool != models.ToolOpenInterpreter {
			t.Errorf("ToolEvents[%d].Tool = %q, want %q", i, e.Tool, models.ToolOpenInterpreter)
		}
		if e.SessionID != "019f6f69-23a0-7e32-bdba-9a9fabc946be" {
			t.Errorf("ToolEvents[%d].SessionID = %q, want the fixture's session id", i, e.SessionID)
		}
		if e.ProjectRoot == "" {
			t.Errorf("ToolEvents[%d].ProjectRoot empty; want cwd (or its resolved git root)", i)
		}
	}

	// Fixture token_count events (last_token_usage, the modern
	// v1.7.24+ per-inference-delta path — see parseModernTokenCount):
	//   turn 1: input=14278 cached=9984 output=39  reasoning=0
	//   turn 2: input=14551 cached=14080 output=176 reasoning=0
	// netInput = input - cached; netOutput = output - reasoning.
	wantTokens := []struct {
		net, out, cached, reasoning int64
	}{
		{net: 14278 - 9984, out: 39, cached: 9984, reasoning: 0},
		{net: 14551 - 14080, out: 176, cached: 14080, reasoning: 0},
	}
	for i, e := range res.TokenEvents {
		if e.Tool != models.ToolOpenInterpreter {
			t.Errorf("TokenEvents[%d].Tool = %q, want %q", i, e.Tool, models.ToolOpenInterpreter)
		}
		if e.Model != "gpt-5.6-sol" {
			t.Errorf("TokenEvents[%d].Model = %q, want %q", i, e.Model, "gpt-5.6-sol")
		}
		w := wantTokens[i]
		if e.InputTokens != w.net {
			t.Errorf("TokenEvents[%d].InputTokens = %d, want %d (gross-cached net)", i, e.InputTokens, w.net)
		}
		if e.OutputTokens != w.out {
			t.Errorf("TokenEvents[%d].OutputTokens = %d, want %d", i, e.OutputTokens, w.out)
		}
		if e.CacheReadTokens != w.cached {
			t.Errorf("TokenEvents[%d].CacheReadTokens = %d, want %d", i, e.CacheReadTokens, w.cached)
		}
		if e.ReasoningTokens != w.reasoning {
			t.Errorf("TokenEvents[%d].ReasoningTokens = %d, want %d", i, e.ReasoningTokens, w.reasoning)
		}
		if e.Source != models.TokenSourceJSONL {
			t.Errorf("TokenEvents[%d].Source = %q, want %q (Tier 2)", i, e.Source, models.TokenSourceJSONL)
		}
		if e.ProjectRoot == "" {
			t.Errorf("TokenEvents[%d].ProjectRoot empty", i)
		}
	}

	// SessionLineages carries no Tool field (nothing to retag) but
	// should still surface the fixture's thread_source:"user" marker
	// unmodified by the retag pass.
	if len(res.SessionLineages) != 1 {
		t.Fatalf("SessionLineages count = %d, want 1", len(res.SessionLineages))
	}
	if res.SessionLineages[0].ThreadSource != "user" {
		t.Errorf("SessionLineages[0].ThreadSource = %q, want %q", res.SessionLineages[0].ThreadSource, "user")
	}
}

// TestOpenInterpreterIncrementalParseRetagsResumedChunk pins that the
// retag seam also applies to incremental (fromOffset > 0) resumes, not
// just a fresh full parse — the resumed chunk goes through the same
// ParseSessionFile wrapper.
func TestOpenInterpreterIncrementalParseRetagsResumedChunk(t *testing.T) {
	t.Parallel()
	dir := oiFixtureDir(t)
	path := oiFixturePath(t)
	a := NewOpenInterpreterWithOptions(nil, dir)

	first, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first.NewOffset == 0 {
		t.Fatal("first parse advanced NewOffset to 0; fixture is non-empty")
	}
	// Resume from partway through the file (after the first
	// token_count event) and confirm the resumed events are still
	// retagged.
	second, err := a.ParseSessionFile(context.Background(), path, first.NewOffset)
	if err != nil {
		t.Fatalf("resume parse: %v", err)
	}
	for i, e := range second.TokenEvents {
		if e.Tool != models.ToolOpenInterpreter {
			t.Errorf("resumed TokenEvents[%d].Tool = %q, want %q", i, e.Tool, models.ToolOpenInterpreter)
		}
	}
}
