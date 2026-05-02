package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupRegistry(t *testing.T) *Registry {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath:    "/opt/observer/bin/observer",
		HomeDir:       home,
		ChecksumsPath: filepath.Join(home, ".observer", "hook_checksums.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegisterClaudeCodeFreshInstall(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	if len(res.HooksAdded) != len(claudeCodeEvents) {
		t.Errorf("added %d want %d", len(res.HooksAdded), len(claudeCodeEvents))
	}

	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, body)
	}
	hooksBlock, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, event := range claudeCodeEvents {
		groups, ok := hooksBlock[event].([]any)
		if !ok || len(groups) == 0 {
			t.Errorf("event %s missing", event)
		}
	}

	// Checksum file should be written.
	csPath := filepath.Join(r.opts.HomeDir, ".observer", "hook_checksums.json")
	if _, err := os.Stat(csPath); err != nil {
		t.Errorf("checksum file not created: %v", err)
	}
}

func TestRegisterClaudeCodeIdempotent(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	if res := r.Register("claude-code"); res.Error != nil {
		t.Fatalf("first: %v", res.Error)
	}
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("second: %v", res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second register added %d (want 0): %v", len(res.HooksAdded), res.HooksAdded)
	}
	if len(res.AlreadySet) != len(claudeCodeEvents) {
		t.Errorf("AlreadySet %d want %d", len(res.AlreadySet), len(claudeCodeEvents))
	}
}

func TestRegisterClaudeCodePreservesOtherKeys(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	// Write a settings.json with unrelated fields that we must not clobber.
	pre := `{"theme":"dark","permissions":{"allow":["bash"]}}`
	path := filepath.Join(r.opts.HomeDir, ".claude", "settings.json")
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, _ := os.ReadFile(path)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["theme"] != "dark" {
		t.Errorf("theme lost: %v", got["theme"])
	}
	if _, ok := got["permissions"]; !ok {
		t.Error("permissions lost")
	}
}

func TestRegisterClaudeCodeConflict(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	path := filepath.Join(r.opts.HomeDir, ".claude", "settings.json")
	pre := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"/usr/local/bin/other-hook"}]}]}}`
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.Register("claude-code")
	if res.Error == nil {
		t.Fatal("expected conflict error")
	}

	// With Force, should succeed and add our hook alongside.
	r.opts.Force = true
	res = r.Register("claude-code")
	if res.Error != nil {
		t.Fatalf("force register: %v", res.Error)
	}
}

func TestRegisterCursorFreshInstall(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	res := r.Register("cursor")
	if res.Error != nil {
		t.Fatalf("Register: %v", res.Error)
	}
	body, err := os.ReadFile(res.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	if got["version"] == nil {
		t.Error("cursor version missing")
	}
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks block missing")
	}
	for _, event := range cursorEvents {
		if _, ok := hooks[event]; !ok {
			t.Errorf("event %s missing", event)
		}
	}
}

func TestRegisterCursorIdempotent(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	_ = r.Register("cursor")
	res := r.Register("cursor")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	if len(res.HooksAdded) != 0 {
		t.Errorf("second add added %d", len(res.HooksAdded))
	}
}

func TestRegisterUnknownTool(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	res := r.Register("notarealthing")
	if res.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	r.opts.DryRun = true
	res := r.Register("claude-code")
	if res.Error != nil {
		t.Fatal(res.Error)
	}
	_, err := os.Stat(res.ConfigPath)
	if err == nil {
		t.Error("dry run wrote the config")
	}
	if !res.DryRun {
		t.Error("result should flag dry run")
	}
}

func TestInstalledDetectsDirs(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(Options{
		BinaryPath: "/x",
		HomeDir:    home,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Installed()
	if len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("Installed = %v want [claude-code]", got)
	}
}

func TestCommandContainsBinaryPath(t *testing.T) {
	t.Parallel()
	r := setupRegistry(t)
	if res := r.Register("claude-code"); res.Error != nil {
		t.Fatal(res.Error)
	}
	body, _ := os.ReadFile(filepath.Join(r.opts.HomeDir, ".claude", "settings.json"))
	if !strings.Contains(string(body), r.opts.BinaryPath) {
		t.Errorf("binary path not in settings: %s", body)
	}
}
