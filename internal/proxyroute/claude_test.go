package proxyroute

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readClaudeSettings parses ~/.claude/settings.json. Helper to keep the
// tests below readable.
func readClaudeSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse settings.json: %v\n%s", err, string(body))
	}
	return out
}

// TestRegisterClaudeCode_FreshFile creates ~/.claude/settings.json from
// scratch and verifies ANTHROPIC_BASE_URL is written under env.
func TestRegisterClaudeCode_FreshFile(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	res := r.RegisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("RegisterClaudeCode: %v", res.Error)
	}
	if !res.Added || res.AlreadySet {
		t.Errorf("expected Added=true AlreadySet=false: %+v", res)
	}
	if res.BaseURL != "http://127.0.0.1:8820" {
		t.Errorf("BaseURL: %q", res.BaseURL)
	}

	settings := readClaudeSettings(t, home)
	env, ok := settings["env"].(map[string]any)
	if !ok {
		t.Fatalf("env not a map: %+v", settings["env"])
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8820" {
		t.Errorf("ANTHROPIC_BASE_URL: %v", env["ANTHROPIC_BASE_URL"])
	}
}

// TestRegisterClaudeCode_PreservesUnrelated round-trips a settings.json
// with hooks + permissions + an unrelated env var and confirms they
// survive.
func TestRegisterClaudeCode_PreservesUnrelated(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	preexisting := map[string]any{
		"hooks":       map[string]any{"PreToolUse": []any{}},
		"permissions": map[string]any{"allow": []any{"Bash(git status)"}},
		"env":         map[string]any{"NODE_OPTIONS": "--max-old-space-size=4096"},
	}
	body, _ := json.MarshalIndent(preexisting, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.RegisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("RegisterClaudeCode: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("expected Added=true: %+v", res)
	}

	settings := readClaudeSettings(t, home)
	if _, ok := settings["hooks"].(map[string]any); !ok {
		t.Error("hooks block lost")
	}
	if _, ok := settings["permissions"].(map[string]any); !ok {
		t.Error("permissions block lost")
	}
	env, ok := settings["env"].(map[string]any)
	if !ok {
		t.Fatalf("env not a map: %+v", settings["env"])
	}
	if env["NODE_OPTIONS"] != "--max-old-space-size=4096" {
		t.Errorf("NODE_OPTIONS lost: %v", env["NODE_OPTIONS"])
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8820" {
		t.Errorf("ANTHROPIC_BASE_URL not added: %v", env["ANTHROPIC_BASE_URL"])
	}
}

// TestRegisterClaudeCode_Idempotent re-running registration on an
// already-pinned settings.json must report AlreadySet without mutating
// the file.
func TestRegisterClaudeCode_Idempotent(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	if res := r.RegisterClaudeCode(); res.Error != nil {
		t.Fatalf("first call: %v", res.Error)
	}
	first, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))

	res := r.RegisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("second call: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("expected AlreadySet=true Added=false: %+v", res)
	}

	second, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if string(first) != string(second) {
		t.Errorf("file mutated between identical calls:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestRegisterClaudeCode_RefusesNonLoopbackWithoutForce protects an
// operator who deliberately set ANTHROPIC_BASE_URL to a third-party
// proxy from silent clobbering on every enrol.
func TestRegisterClaudeCode_RefusesNonLoopbackWithoutForce(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(map[string]any{
		"env": map[string]any{"ANTHROPIC_BASE_URL": "https://proxy.example.com"},
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.RegisterClaudeCode()
	if res.Error == nil {
		t.Fatal("expected error for non-loopback ANTHROPIC_BASE_URL without --force")
	}
	if res.Added {
		t.Errorf("must not Added on conflict: %+v", res)
	}

	settings := readClaudeSettings(t, home)
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://proxy.example.com" {
		t.Errorf("file was mutated on refuse: %v", env["ANTHROPIC_BASE_URL"])
	}
}

// TestRegisterClaudeCode_PreservesAnotherObserver — if the user has
// another observer install on a different port, we don't clobber it
// (treat as AlreadySet) unless --force.
func TestRegisterClaudeCode_PreservesAnotherObserver(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(map[string]any{
		"env": map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:9999"},
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.RegisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("loopback should not error without --force: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("expected AlreadySet=true (preserved another observer): %+v", res)
	}
	if res.BaseURL != "http://127.0.0.1:9999" {
		t.Errorf("BaseURL should reflect the preserved value: %q", res.BaseURL)
	}
}

// TestUnregisterClaudeCode_RemovesOwnEntry: drops ANTHROPIC_BASE_URL
// from settings.json when it points at a loopback (our observer).
func TestUnregisterClaudeCode_RemovesOwnEntry(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	if res := r.RegisterClaudeCode(); res.Error != nil {
		t.Fatalf("register: %v", res.Error)
	}

	res := r.UnregisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	if !res.Added {
		t.Errorf("expected Added=true (removed), got %+v", res)
	}

	settings := readClaudeSettings(t, home)
	if env, ok := settings["env"].(map[string]any); ok {
		if v, hit := env["ANTHROPIC_BASE_URL"]; hit {
			t.Errorf("ANTHROPIC_BASE_URL not removed: %v", v)
		}
	}
}

// TestUnregisterClaudeCode_PreservesThirdParty must NOT touch a
// non-loopback value the operator chose deliberately.
func TestUnregisterClaudeCode_PreservesThirdParty(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(map[string]any{
		"env": map[string]any{"ANTHROPIC_BASE_URL": "https://proxy.example.com"},
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.UnregisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	if res.Added {
		t.Errorf("must not remove a third-party entry: %+v", res)
	}
	settings := readClaudeSettings(t, home)
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://proxy.example.com" {
		t.Errorf("third-party value mutated: %v", env["ANTHROPIC_BASE_URL"])
	}
}

// TestUnregisterClaudeCode_AbsentFile: no settings.json → noop, no
// error, AlreadySet=true.
func TestUnregisterClaudeCode_AbsentFile(t *testing.T) {
	r, _ := newRegistrar(t, 8820, false)
	res := r.UnregisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	if !res.AlreadySet || res.Added {
		t.Errorf("expected AlreadySet=true Added=false on absent file: %+v", res)
	}
}

// TestUnregisterClaudeCode_DropsEmptyEnvBlock: when the env map
// becomes empty after removal, the env key itself should disappear
// (symmetric with RegisterClaudeCode's "create env" case).
func TestUnregisterClaudeCode_DropsEmptyEnvBlock(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	if res := r.RegisterClaudeCode(); res.Error != nil {
		t.Fatalf("register: %v", res.Error)
	}
	if res := r.UnregisterClaudeCode(); res.Error != nil {
		t.Fatalf("unregister: %v", res.Error)
	}
	settings := readClaudeSettings(t, home)
	if _, ok := settings["env"]; ok {
		t.Errorf("env block should be removed when it becomes empty, got: %+v", settings["env"])
	}
}

// TestUnregisterClaudeCode_PreservesSiblingEnvKeys: when env carries
// other keys (e.g. NODE_OPTIONS), only ANTHROPIC_BASE_URL is removed.
func TestUnregisterClaudeCode_PreservesSiblingEnvKeys(t *testing.T) {
	r, home := newRegistrar(t, 8820, false)
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(map[string]any{
		"env": map[string]any{
			"NODE_OPTIONS":       "--max-old-space-size=4096",
			"ANTHROPIC_BASE_URL": "http://127.0.0.1:8820",
		},
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := r.UnregisterClaudeCode()
	if res.Error != nil || !res.Added {
		t.Fatalf("expected clean Added=true: %+v", res)
	}
	settings := readClaudeSettings(t, home)
	env, ok := settings["env"].(map[string]any)
	if !ok {
		t.Fatal("env block dropped despite NODE_OPTIONS sibling")
	}
	if env["NODE_OPTIONS"] != "--max-old-space-size=4096" {
		t.Errorf("NODE_OPTIONS mutated: %v", env["NODE_OPTIONS"])
	}
	if _, has := env["ANTHROPIC_BASE_URL"]; has {
		t.Errorf("ANTHROPIC_BASE_URL not removed: %v", env["ANTHROPIC_BASE_URL"])
	}
}

// TestRegisterClaudeCode_DryRun: --dry-run must not touch disk.
func TestRegisterClaudeCode_DryRun(t *testing.T) {
	home := t.TempDir()
	r, err := NewRegistrar(RegisterOptions{
		ProxyPort: 8820,
		HomeDir:   home,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	res := r.RegisterClaudeCode()
	if res.Error != nil {
		t.Fatalf("RegisterClaudeCode: %v", res.Error)
	}
	if !res.Added || !res.DryRun {
		t.Errorf("expected Added=true DryRun=true: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote settings.json: %v", err)
	}
}
