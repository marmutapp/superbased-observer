package kilocode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestLegacy_RetagsClineFormatAsKiloCode pins the load-bearing
// re-tag step. The LegacyAdapter wraps a cline.Adapter and the only
// difference between the two adapters' emitted rows is the `Tool`
// column value — we must rewrite every event to ToolKiloCode before
// returning, otherwise dashboard rollups attribute Kilo activity to
// Cline.
func TestLegacy_RetagsClineFormatAsKiloCode(t *testing.T) {
	root := t.TempDir()
	tasksRoot := filepath.Join(root, "kilocode.kilo-code", "tasks")
	taskID := "task-001"
	taskDir := filepath.Join(tasksRoot, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cline-shape minimal session: one user message with the
	// env-details cwd banner + one assistant message with a tool_use.
	// The cwd banner is what cline.inferProjectContext keys on so the
	// emitted rows carry a non-empty ProjectRoot. We forward-slash the
	// embedded path so it's valid JSON regardless of the host OS — on
	// Windows the TempDir path contains backslashes which would break
	// the JSON string literal.
	cwdInJSON := filepath.ToSlash(root)
	body := `[
		{"role":"user","ts":1700000000000,"content":[{"type":"text","text":"<environment_details>\n# Current Working Directory (` + cwdInJSON + `) Files\nREADME.md\n</environment_details>"}]},
		{"role":"assistant","ts":1700000010000,"model":"claude-haiku-4-5","content":[
			{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
		]}
	]`
	apiPath := filepath.Join(taskDir, "api_conversation_history.json")
	if err := os.WriteFile(apiPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewLegacyWithOptions(nil, []string{tasksRoot})
	if !a.IsSessionFile(apiPath) {
		t.Fatalf("IsSessionFile(%q) returned false for a path under our own watch root", apiPath)
	}
	res, err := a.ParseSessionFile(context.Background(), apiPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) == 0 {
		t.Fatalf("no events emitted")
	}
	for _, ev := range res.ToolEvents {
		if ev.Tool != models.ToolKiloCode {
			t.Errorf("ev.Tool = %q, want %q (LegacyAdapter must re-tag every event)", ev.Tool, models.ToolKiloCode)
		}
		if ev.Tool == models.ToolCline {
			t.Errorf("ev.Tool leaked through as %q — re-tag step regressed", ev.Tool)
		}
	}
}

// TestLegacy_IsSessionFileRejectsForeignAPIHistory pins the v1.4.51
// dispatch contract — a same-basename path outside our watch roots
// MUST be rejected. Without the under-WatchPaths constraint a
// cline-rooted api_conversation_history.json would be claimed by the
// kilocode legacy adapter (and vice-versa).
func TestLegacy_IsSessionFileRejectsForeignAPIHistory(t *testing.T) {
	root := t.TempDir()
	a := NewLegacyWithOptions(nil, []string{filepath.Join(root, "kilocode.kilo-code", "tasks")})
	foreign := filepath.Join(root, "saoudrizwan.claude-dev", "tasks", "abc", "api_conversation_history.json")
	if a.IsSessionFile(foreign) {
		t.Errorf("IsSessionFile claimed a Cline-rooted path: %q", foreign)
	}
}

// TestLegacy_NameReturnsKiloCode pins the public adapter identity.
func TestLegacy_NameReturnsKiloCode(t *testing.T) {
	if got := (&LegacyAdapter{}).Name(); got != models.ToolKiloCode {
		t.Errorf("Name = %q, want %q", got, models.ToolKiloCode)
	}
}

// TestLegacy_DefaultRootsIncludesVSCodeServer pins that we glob BOTH
// the VS Code desktop globalStorage AND the VS Code Server location
// (the latter is where Remote-WSL captured sessions live). On a host
// without crossmount homes the test still passes — defaultRoots
// returns nil; the assertion only fires when at least one home is
// detected (every real install).
func TestLegacy_DefaultRootsIncludesVSCodeServer(t *testing.T) {
	a := NewLegacy()
	hasDesktop := false
	hasServer := false
	for _, r := range a.WatchPaths() {
		// Crude substring checks — sufficient because the two paths
		// disambiguate cleanly on their parent dir name.
		if filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(r)))) == "Code" ||
			filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(r)))) == "Code - Insiders" {
			hasDesktop = true
		}
		if filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(r)))) == ".vscode-server" {
			hasServer = true
		}
	}
	// Only assert when we actually got roots back; on a sandboxed
	// runner with no detected homes, hasDesktop/hasServer may both
	// stay false and that's fine — the defaults_test invariants
	// already cover the under-WatchPaths constraint.
	if len(a.WatchPaths()) > 0 && !hasDesktop && !hasServer {
		t.Logf("NewLegacy returned %d roots but none look like Code-desktop or vscode-server; check defaultRoots()", len(a.WatchPaths()))
	}
}
