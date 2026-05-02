package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// newRegistrar wires a sandboxed Registrar against tmpHome so tests don't
// touch the real user config.
func newRegistrar(t *testing.T, force bool) (*Registrar, string) {
	t.Helper()
	home := t.TempDir()
	r, err := NewRegistrar(RegisterOptions{
		BinaryPath: "/usr/local/bin/observer",
		HomeDir:    home,
		Force:      force,
	})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	return r, home
}

func TestRegistrar_NewValidation(t *testing.T) {
	if _, err := NewRegistrar(RegisterOptions{}); err == nil {
		t.Error("missing BinaryPath should error")
	}
}

func TestRegistrar_Installed(t *testing.T) {
	r, home := newRegistrar(t, false)
	if got := r.Installed(); len(got) != 0 {
		t.Errorf("fresh home: %v", got)
	}
	for _, d := range []string{".claude", ".cursor", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Installed()
	want := map[string]bool{"claude-code": true, "cursor": true, "codex": true}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected: %s", g)
		}
		delete(want, g)
	}
	if len(want) != 0 {
		t.Errorf("missing: %v", want)
	}
}

func TestRegistrar_ClaudeCodeJSON(t *testing.T) {
	r, home := newRegistrar(t, false)
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("expected Added=true: %+v", res)
	}
	if res.ConfigPath != filepath.Join(home, ".claude.json") {
		t.Errorf("ConfigPath: %s", res.ConfigPath)
	}

	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := top["mcpServers"].(map[string]any)
	entry := servers[ServerName].(map[string]any)
	if entry["command"] != "/usr/local/bin/observer" {
		t.Errorf("command: %v", entry["command"])
	}
	args := entry["args"].([]any)
	if len(args) != 1 || args[0] != "serve" {
		t.Errorf("args: %v", args)
	}

	// Idempotent re-register.
	res2 := r.Register("claude-code")
	if res2.Error != nil {
		t.Fatalf("re-register: %v", res2.Error)
	}
	if !res2.AlreadySet {
		t.Errorf("expected AlreadySet on re-register: %+v", res2)
	}
	if res2.Added {
		t.Errorf("expected Added=false on re-register")
	}
}

func TestRegistrar_CursorJSON(t *testing.T) {
	r, home := newRegistrar(t, false)
	res := r.Register("cursor")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	want := filepath.Join(home, ".cursor", "mcp.json")
	if res.ConfigPath != want {
		t.Errorf("ConfigPath: %s want %s", res.ConfigPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func TestRegistrar_PreservesExistingMCPServers(t *testing.T) {
	r, home := newRegistrar(t, false)
	// Pre-seed Claude config with another MCP server we shouldn't clobber.
	path := filepath.Join(home, ".claude.json")
	prior := `{
  "feedbackSurveyState": {"shown": true},
  "mcpServers": {
    "other-server": {"command": "/opt/other", "args": ["serve"]}
  }
}`
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, _ := os.ReadFile(path)
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("parse: %v: %s", err, body)
	}
	if _, ok := top["feedbackSurveyState"]; !ok {
		t.Error("feedbackSurveyState was clobbered")
	}
	servers := top["mcpServers"].(map[string]any)
	if _, ok := servers["other-server"]; !ok {
		t.Errorf("other-server lost: %v", servers)
	}
	if _, ok := servers[ServerName]; !ok {
		t.Errorf("our server missing: %v", servers)
	}
}

func TestRegistrar_ConflictRequiresForce(t *testing.T) {
	r, home := newRegistrar(t, false)
	path := filepath.Join(home, ".claude.json")
	prior := `{"mcpServers":{"observer":{"command":"/different/path","args":["x"]}}}`
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	res := r.Register("claude-code")
	if res.Error == nil {
		t.Errorf("expected conflict error: %+v", res)
	}
	if !strings.Contains(res.Error.Error(), "--force") {
		t.Errorf("error should mention --force: %v", res.Error)
	}

	// With force=true, conflict is overwritten.
	rf, _ := NewRegistrar(RegisterOptions{
		BinaryPath: "/usr/local/bin/observer",
		HomeDir:    home,
		Force:      true,
	})
	res2 := rf.Register("claude-code")
	if res2.Error != nil {
		t.Fatalf("force register: %v", res2.Error)
	}
	if !res2.Added {
		t.Errorf("force should overwrite: %+v", res2)
	}
}

func TestRegistrar_DryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	r, _ := NewRegistrar(RegisterOptions{
		BinaryPath: "/usr/local/bin/observer",
		HomeDir:    home,
		DryRun:     true,
	})
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if !res.DryRun || !res.Added {
		t.Errorf("expected DryRun+Added: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("file written despite dry-run")
	}
}

func TestRegistrar_CodexTOML(t *testing.T) {
	r, home := newRegistrar(t, false)
	// Pre-seed a config.toml with an unrelated section we must preserve.
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	prior := "model = \"o4-mini\"\n\n[mcp_servers.other]\ncommand = \"/opt/other\"\nargs = [\"serve\"]\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	res := r.Register("codex")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("expected Added: %+v", res)
	}

	body, _ := os.ReadFile(path)
	var root map[string]any
	if err := toml.Unmarshal(body, &root); err != nil {
		t.Fatalf("re-parse: %v: %s", err, body)
	}
	if root["model"] != "o4-mini" {
		t.Errorf("top-level model lost: %v", root["model"])
	}
	servers := root["mcp_servers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Errorf("other server lost: %v", servers)
	}
	ours := servers[ServerName].(map[string]any)
	if ours["command"] != "/usr/local/bin/observer" {
		t.Errorf("command: %v", ours["command"])
	}

	// Re-register is idempotent.
	res2 := r.Register("codex")
	if res2.Error != nil {
		t.Fatalf("re-register: %v", res2.Error)
	}
	if !res2.AlreadySet {
		t.Errorf("expected AlreadySet: %+v", res2)
	}
}

func TestRegistrar_UnknownTool(t *testing.T) {
	r, _ := newRegistrar(t, false)
	res := r.Register("nonexistent")
	if res.Error == nil {
		t.Error("expected error for unknown tool")
	}
}
