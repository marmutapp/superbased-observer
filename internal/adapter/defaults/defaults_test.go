package defaults

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// shapeFixturePath returns a path with the shape each adapter's
// IsSessionFile predicate would otherwise accept (extension +
// basename + path substrings) but rooted under /tmp/foreign rather
// than the adapter's actual WatchPaths. The v1.4.51 invariant is
// that every adapter must REJECT such a path because the
// under-WatchPaths constraint isn't satisfied — even though the
// shape filter alone would accept.
//
// Mapping is keyed on adapter.Name(); a new adapter that joins the
// default set without a fixture entry here triggers an explicit
// t.Fatalf in TestAllAdapters_IsSessionFile_RequiresUnderWatchRoots.
// That's intentional: the fixture map is the source of truth for
// "what does each predicate look like?" and forces the author of a
// new adapter to think about it.
var shapeFixturePathByAdapter = map[string]string{
	"claude-code":     "/tmp/foreign/x.jsonl",
	"codex":           "/tmp/foreign/rollout-2026-05-13-abc.jsonl",
	"cline":           "/tmp/foreign/tasks/abc/api_conversation_history.json",
	"cline-cli":       "/tmp/foreign/.cline/data/db/sessions.db",
	"copilot":         "/tmp/foreign/workspaceStorage/ws/chatSessions/sess.jsonl",
	"copilot-cli":     "/tmp/foreign/session-state/9da4aa10-1da9-49a5-931f-f89c2528c6db/events.jsonl",
	"cowork":          "/tmp/foreign/local_abc-123-def/audit.jsonl",
	"cursor":          "/tmp/foreign/.cursor/projects/slug/agent-transcripts/abc/abc.jsonl",
	"gemini-cli":      "/tmp/foreign/.gemini/tmp/abc/chats/session-1.jsonl",
	"opencode":        "/tmp/foreign/opencode.db",
	"openclaw":        "/tmp/foreign/tasks/runs.sqlite",
	"pi":              "/tmp/foreign/some.jsonl",
	"antigravity":     "/tmp/foreign/.gemini/antigravity/conversations/abc.pb",
	"antigravity-cli": "/tmp/foreign/.gemini/antigravity-cli/conversations/abc.db",
	"hermes":          "/tmp/foreign/.hermes/state.db",
	// Legacy Kilo Code IDE extension — same basename as cline
	// (api_conversation_history.json) but under the
	// `kilocode.kilo-code/tasks/` subdir instead of
	// `saoudrizwan.claude-dev/tasks/`. The under-WatchPaths constraint
	// is what stops a cline-shaped foreign path from being claimed by
	// this adapter and vice-versa.
	"kilo-code": "/tmp/foreign/Code/User/globalStorage/kilocode.kilo-code/tasks/abc/api_conversation_history.json",
	// Kilo Code CLI — SQLite store under ~/.local/share/kilo/.
	// Mirrors the opencode fixture shape because the basename predicate
	// is structurally identical (kilo.db / kilo.db-wal).
	"kilo-code-cli": "/tmp/foreign/.local/share/kilo/kilo.db",
	// Qwen Code — CC-shaped JSONL under .qwen/projects/<slug>/chats/.
	"qwen-code": "/tmp/foreign/.qwen/projects/-tmp-foo/chats/4dcd44ae-1111-2222-3333-444444444444.jsonl",
	// Kiro CLI — flat interactive bundle under .kiro/sessions/cli/
	// (the sqlite conversations_v2 layout shares the same root-gating).
	"kiro-cli": "/tmp/foreign/.kiro/sessions/cli/257b488d-d9bf-42bc-bb07-4609ac0bca39.json",
	// Crush — project-local .crush/crush.db; root-gating comes from the
	// projects.json-discovered root set, so a foreign .crush dir must
	// be rejected.
	"crush": "/tmp/foreign/.crush/crush.db",
	// Kimi Code — wire.jsonl under .kimi-code/sessions/<wd>/<session>/
	// agents/<agent>/. The shape predicate deliberately does NOT bind
	// the literal `.kimi-code` substring; the under-WatchPaths gate is
	// the sole install-root authority.
	"kimi-code": "/tmp/foreign/.kimi-code/sessions/wd_x_0000/session_y/agents/main/wire.jsonl",
	// Grok — ACP updates.jsonl under .grok/sessions/<url-enc-cwd>/<uuid>/
	// (the token source, logs/unified.jsonl, shares the same root-gating).
	"grok": "/tmp/foreign/.grok/sessions/%2Fhome%2Fx%2Fproj/2e0a4a70-0000-0000-0000-000000000000/updates.jsonl",
	// Devin CLI — SQLite sessions.db whose immediate parent dir must be
	// `cli` (the Cognition layout ~/.local/share/devin/cli/sessions.db;
	// Windows %APPDATA%\devin\cli\). Root-gating rejects a foreign
	// cli/sessions.db.
	"devin": "/tmp/foreign/.local/share/devin/cli/sessions.db",
	// Qoder CLI — CC-shaped JSONL under .qoder/projects/<dash-slug>/
	// (the token-bearing run segments under .qoder/logs/sessions/ share
	// the same root-gating).
	"qoder": "/tmp/foreign/.qoder/projects/-tmp-foo/5d51bc6a-0000-0000-0000-000000000000.jsonl",
	// Aider — per-repo Markdown transcript at the git root; watch roots
	// are DISCOVERED (bounded home walk), so a same-named file in a
	// foreign repo must be rejected.
	"aider": "/tmp/foreign/somerepo/.aider.chat.history.md",
	// goose — WAL SQLite sessions.db whose immediate parent dir must be
	// `sessions` (the Block layout ~/.local/share/goose/sessions/;
	// Windows %APPDATA%\Block\goose\data\sessions\). Root-gating
	// rejects a foreign sessions/sessions.db.
	"goose": "/tmp/foreign/.local/share/goose/sessions/sessions.db",
	// droid (Factory AI) — <uuid>.jsonl under .factory/sessions/<dir>/.
	// The shape predicate binds the `/.factory/sessions/` segment, so the
	// fixture repeats it below /tmp/foreign; the sibling
	// <uuid>.settings.json sidecar is rejected by the .jsonl suffix.
	"droid": "/tmp/foreign/.factory/sessions/proj/1b6f2f3c-0000-0000-0000-000000000000.jsonl",
	// open-interpreter — the rebadged Codex CLI Rust build, so the shape
	// predicate is codex's verbatim (rollout-*.jsonl). Only the watch root
	// differs (.openinterpreter/sessions vs .codex/sessions), which is
	// exactly what this test proves: a foreign rollout file is rejected,
	// and the codex/open-interpreter split is root-based, not shape-based.
	"open-interpreter": "/tmp/foreign/rollout-2026-07-29-oi.jsonl",
	// command-code — <uuid>.jsonl under .commandcode/projects/<slug>/.
	// The <uuid>.checkpoints.jsonl sidecar shares the extension and is
	// rejected by the adapter's own suffix test, not by the root gate.
	"command-code": "/tmp/foreign/.commandcode/projects/-tmp-foo/9f4c1a20-0000-0000-0000-000000000000.jsonl",
	// chatgpt-web — hook-only browser adapter with NO watch paths and an
	// IsSessionFile that always returns false. Any shaped path is rejected
	// (there is no on-disk session file), so the reject test passes and the
	// accept test skips (no WatchPaths in any environment). The fixture is
	// required only because the map is the source of truth for every
	// registered adapter.
	"chatgpt-web": "/tmp/foreign/browser/chatgpt-web/turn.json",
	// The other *-web browser adapters are identically hook-only (no watch
	// paths, IsSessionFile always false). Same rationale as chatgpt-web: the
	// fixture is required only because the map is the source of truth for
	// every registered adapter.
	"claude-web":     "/tmp/foreign/browser/claude-web/turn.json",
	"perplexity-web": "/tmp/foreign/browser/perplexity-web/turn.json",
	"gemini-web":     "/tmp/foreign/browser/gemini-web/turn.json",
	"copilot-web":    "/tmp/foreign/browser/copilot-web/turn.json",
}

// TestAllAdapters_IsSessionFile_RequiresUnderWatchRoots is the
// load-bearing v1.4.51 invariant: for every adapter in the default
// set, IsSessionFile MUST reject a path that has the right shape but
// lives outside the adapter's WatchPaths. Removes the entire
// alphabetical-sort misrouting bug class — any new adapter that
// regresses this invariant fails CI immediately.
func TestAllAdapters_IsSessionFile_RequiresUnderWatchRoots(t *testing.T) {
	for _, a := range Adapters() {
		name := a.Name()
		fixture, ok := shapeFixturePathByAdapter[name]
		if !ok {
			t.Fatalf("adapter %q joined the default set without a shapeFixturePathByAdapter entry — add one that has the right shape but lives at /tmp/foreign/...", name)
		}
		t.Run(name, func(t *testing.T) {
			if a.IsSessionFile(fixture) {
				t.Errorf("IsSessionFile(%q) returned true — adapter %s claims a foreign path. The under-WatchPaths constraint must reject anything outside the adapter's own watch roots.", fixture, name)
			}
		})
	}
}

// TestAllAdapters_IsSessionFile_AcceptsUnderWatchRoots is the positive
// counterpart: for every adapter, a same-shape path constructed
// under one of the adapter's own WatchPaths MUST be accepted. Without
// this we couldn't tell whether the under-WatchPaths constraint is
// broken vs the shape filter being too narrow.
func TestAllAdapters_IsSessionFile_AcceptsUnderWatchRoots(t *testing.T) {
	for _, a := range Adapters() {
		name := a.Name()
		fixture, ok := shapeFixturePathByAdapter[name]
		if !ok {
			t.Fatalf("missing shape fixture for adapter %q", name)
		}
		// Derive the relative session-shape suffix from the
		// /tmp/foreign-rooted fixture and join it under each of the
		// adapter's real watch roots. If the adapter has no detected
		// watch roots in this environment (e.g. no $HOME/.codex on
		// CI), skip — the invariant test still proves the
		// constraint is enforced.
		suffix := strings.TrimPrefix(fixture, "/tmp/foreign/")
		roots := a.WatchPaths()
		if len(roots) == 0 {
			t.Run(name+"_skip_no_roots", func(t *testing.T) {
				t.Skipf("adapter %s has no detected WatchPaths in this environment", name)
			})
			continue
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(roots[0], suffix)
			if !a.IsSessionFile(path) {
				t.Errorf("IsSessionFile(%q) returned false for a path under %s's own watch root %s — predicate is over-constrained", path, name, roots[0])
			}
		})
	}
}

// TestRegistryRootsNonOverlapping verifies that no two distinct
// adapters' WatchPaths share a prefix. Root-based dispatch is
// unambiguous only when roots are pairwise non-prefix — if two
// adapters ever watched overlapping trees, longest-prefix dispatch
// would still pick one of them, but the choice would be silently
// dependent on map iteration order. Future-proofing.
//
// Same-adapter overlap is allowed (crossmount can return nested
// roots: ~/.claude/projects AND /mnt/c/Users/.../.claude/projects).
func TestRegistryRootsNonOverlapping(t *testing.T) {
	type rootEntry struct {
		adapter adapter.Adapter
		root    string
	}
	var all []rootEntry
	for _, a := range Adapters() {
		for _, r := range a.WatchPaths() {
			if r == "" {
				continue
			}
			all = append(all, rootEntry{a, r})
		}
	}
	for i := range all {
		for j := range all {
			if i == j {
				continue
			}
			if all[i].adapter.Name() == all[j].adapter.Name() {
				continue
			}
			if adapter.HasPathPrefix(all[j].root, all[i].root) {
				t.Errorf("watch-root overlap: adapter %s root %q is a prefix of adapter %s root %q. Root-based dispatch becomes ambiguous; tighten one adapter's WatchPaths or upgrade to Option B (parse_cursors.adapter column).",
					all[i].adapter.Name(), all[i].root,
					all[j].adapter.Name(), all[j].root)
			}
		}
	}
}
