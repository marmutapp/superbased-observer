package arena

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sandboxSettingsFixture() []byte {
	return []byte(`{
  "env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8820"},
  "model": "claude-fable-5[1m]",
  "permissions": {
    "deny": [
      "Bash(mkfs:*)",
      "Edit(~/.ssh/**)",
      "Write(~/.ssh/**)",
      "Edit(~/.observer/**)",
      "Write(~/.observer/**)",
      "Write(~/.codex/config.toml)"
    ],
    "ask": ["Read(**/.env*)"]
  },
  "hooks": {"Stop": [{"matcher": "*", "hooks": [{"type": "command", "command": "/bin/observer hook"}]}]}
}`)
}

func TestFilterClaudeDenyRulesDropsWorkspaceDenies(t *testing.T) {
	t.Parallel()
	home := "/home/op"
	ws := filepath.Join(home, ".observer", "arena")
	out, err := filterClaudeDenyRules(sandboxSettingsFixture(), home, ws)
	if err != nil {
		t.Fatalf("filterClaudeDenyRules: %v", err)
	}
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
			Ask  []string `json:"ask"`
		} `json:"permissions"`
		Hooks json.RawMessage `json:"hooks"`
		Model string          `json:"model"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("filtered settings not valid JSON: %v", err)
	}
	for _, rule := range doc.Permissions.Deny {
		if strings.Contains(rule, "~/.observer") {
			t.Fatalf("workspace deny survived filtering: %q", rule)
		}
	}
	got := strings.Join(doc.Permissions.Deny, "|")
	for _, want := range []string{"Bash(mkfs:*)", "Edit(~/.ssh/**)", "Write(~/.codex/config.toml)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("unrelated deny dropped: want %q in %q", want, got)
		}
	}
	if len(doc.Permissions.Ask) != 1 || doc.Permissions.Ask[0] != "Read(**/.env*)" {
		t.Fatalf("ask list not preserved: %v", doc.Permissions.Ask)
	}
	if len(doc.Hooks) == 0 || doc.Model != "claude-fable-5[1m]" {
		t.Fatal("hooks/model preferences not preserved")
	}
}

func TestFilterClaudeDenyRulesNoPermissionsBlock(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"env":{"A":"b"}}`)
	out, err := filterClaudeDenyRules(raw, "/home/op", "/home/op/.observer/arena")
	if err != nil {
		t.Fatalf("filterClaudeDenyRules: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if _, ok := doc["permissions"]; ok {
		t.Fatal("permissions block invented for settings without one")
	}
}

func TestDenyRuleCovers(t *testing.T) {
	t.Parallel()
	home := "/home/op"
	cases := []struct {
		rule    string
		ws      string
		covered bool
	}{
		{"Edit(~/.observer/**)", home + "/.observer/arena", true},
		{"Write(~/.observer)", home + "/.observer/arena", true},
		{"Edit(~/.observer/arena)", home + "/.observer/arena", true},
		{"Write(/srv/other/**)", home + "/.observer/arena", false},
		{"Edit(~/.ssh/**)", home + "/.observer/arena", false},
		{"Bash(mkfs:*)", home + "/.observer/arena", false}, // no parens-wrapped path glob match
		{"garbage rule", home + "/.observer/arena", false},
	}
	for _, tc := range cases {
		if got := denyRuleCovers(tc.rule, home, tc.ws); got != tc.covered {
			t.Errorf("denyRuleCovers(%q) = %v, want %v", tc.rule, got, tc.covered)
		}
	}
}

func TestPrepareClaudeSandbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), sandboxSettingsFixture(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", credentialsFileName), []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	ws := filepath.Join(home, ".observer", "arena")

	cfgDir, err := prepareClaudeSandbox(runDir, "claude-code", ws)
	if err != nil {
		t.Fatalf("prepareClaudeSandbox: %v", err)
	}
	if cfgDir != filepath.Join(runDir, ".claude-code-claude-cfg") {
		t.Fatalf("unexpected cfg dir layout: %s", cfgDir)
	}
	raw, err := os.ReadFile(filepath.Join(cfgDir, "settings.json"))
	if err != nil {
		t.Fatalf("sandbox settings missing: %v", err)
	}
	if strings.Contains(string(raw), "~/.observer") {
		t.Fatal("workspace deny leaked into sandbox settings")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, credentialsFileName)); err != nil {
		t.Fatalf("credentials not copied: %v", err)
	}
}

func TestPrepareClaudeSandboxMissingHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runDir := t.TempDir()
	cfgDir, err := prepareClaudeSandbox(runDir, "judge", filepath.Join(home, ".observer", "arena"))
	if err != nil {
		t.Fatalf("missing global config must degrade, got: %v", err)
	}
	if fi, err := os.Stat(cfgDir); err != nil || !fi.IsDir() {
		t.Fatalf("cfg dir not created: %v %v", fi, err)
	}
}
