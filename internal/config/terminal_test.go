package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTerminalDefaults(t *testing.T) {
	c := Default()
	if !c.Terminal.Enabled {
		t.Error("terminal should be enabled by default")
	}
	if !c.Terminal.Status.Enabled {
		t.Error("terminal status should be enabled by default")
	}
	// Fresh launch is a conscious opt-in — OFF by default.
	if c.Terminal.Launch.AllowFreshAgent {
		t.Error("fresh-agent launch must default OFF (privilege expansion)")
	}
	if len(c.Terminal.Launch.AllowedTools) != 0 || len(c.Terminal.Launch.AllowedProjectRoots) != 0 {
		t.Error("fresh-launch allow-lists must default empty (deny-all)")
	}
	if c.Terminal.MaxConcurrent != 4 || c.Terminal.IdleTimeout != "30m" {
		t.Errorf("terminal bounds = %+v", c.Terminal)
	}
}

func TestTerminalPartialMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[terminal.launch]
allow_fresh_agent = true
allowed_tools = ["claude-code", "codex"]
allowed_project_roots = ["/home/dev/projects"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The opt-in flipped, but the unset terminal-wide knobs kept their
	// Default() seed (partial-merge invariant).
	if !cfg.Terminal.Launch.AllowFreshAgent {
		t.Error("allow_fresh_agent did not flip on")
	}
	if len(cfg.Terminal.Launch.AllowedTools) != 2 {
		t.Errorf("allowed_tools = %v", cfg.Terminal.Launch.AllowedTools)
	}
	if !cfg.Terminal.Enabled || cfg.Terminal.MaxConcurrent != 4 {
		t.Errorf("terminal-wide defaults lost on partial merge: %+v", cfg.Terminal)
	}
}

func TestTerminalValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid default", func(*Config) {}, false},
		{"negative max_concurrent", func(c *Config) { c.Terminal.MaxConcurrent = -1 }, true},
		{"negative ring_bytes", func(c *Config) { c.Terminal.RingBytes = -1 }, true},
		{"bad idle_timeout", func(c *Config) { c.Terminal.IdleTimeout = "not-a-duration" }, true},
		{"empty idle_timeout ok", func(c *Config) { c.Terminal.IdleTimeout = "" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			err := Validate(c)
			if tc.wantErr && err == nil {
				t.Error("expected validation error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
