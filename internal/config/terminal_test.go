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
	// IdleTimeout "0" = idle reaping DISABLED by default (continuity):
	// a live session stays until its child exits or an explicit close.
	if c.Terminal.MaxConcurrent != 9 || c.Terminal.IdleTimeout != "0" {
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
	if !cfg.Terminal.Enabled || cfg.Terminal.MaxConcurrent != 9 {
		t.Errorf("terminal-wide defaults lost on partial merge: %+v", cfg.Terminal)
	}
}

// TestTerminalAttachDefaultOnCompat pins the resilient-attach compat trap:
// an EXISTING [terminal.attach] block that carries enabled/route_proxy but was
// written BEFORE the default_on key existed must still load with DefaultOn=true.
// This works because DefaultOn is seeded true in Default() and BurntSushi's
// field-level decode leaves the absent key untouched (the same partial-merge
// mechanism RouteProxy relies on). An explicit default_on = false must stick.
func TestTerminalAttachDefaultOnCompat(t *testing.T) {
	// Fresh Default() carries DefaultOn=true.
	if !Default().Terminal.Attach.DefaultOn {
		t.Fatal("Default() must seed Attach.DefaultOn=true")
	}

	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			// The compat trap: a legacy block with no default_on key.
			name: "legacy block without default_on keeps DefaultOn=true",
			body: "[terminal.attach]\nenabled = true\n",
			want: true,
		},
		{
			name: "legacy block with route_proxy but no default_on keeps true",
			body: "[terminal.attach]\nenabled = true\nroute_proxy = false\n",
			want: true,
		},
		{
			name: "explicit default_on = false sticks",
			body: "[terminal.attach]\nenabled = true\ndefault_on = false\n",
			want: false,
		},
		{
			name: "explicit default_on = true sticks",
			body: "[terminal.attach]\ndefault_on = true\n",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Terminal.Attach.DefaultOn; got != tc.want {
				t.Errorf("Attach.DefaultOn = %v, want %v", got, tc.want)
			}
			// The attach block stays default-ON regardless.
			if !cfg.Terminal.Attach.Enabled {
				t.Errorf("Attach.Enabled lost its default: %+v", cfg.Terminal.Attach)
			}
		})
	}
}

// TestTerminalAttachReclaimOnInputCompat pins the same compat trap for the
// Feature-1 reclaim_on_input key: an EXISTING [terminal.attach] block written
// BEFORE the key existed must still load with ReclaimOnInput=true (seeded true
// in Default(); BurntSushi leaves an absent key untouched — the same mechanism
// DefaultOn / RouteProxy rely on). An explicit reclaim_on_input = false sticks.
func TestTerminalAttachReclaimOnInputCompat(t *testing.T) {
	if !Default().Terminal.Attach.ReclaimOnInput {
		t.Fatal("Default() must seed Attach.ReclaimOnInput=true")
	}

	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "legacy block without reclaim_on_input keeps true",
			body: "[terminal.attach]\nenabled = true\n",
			want: true,
		},
		{
			name: "legacy block with default_on but no reclaim_on_input keeps true",
			body: "[terminal.attach]\ndefault_on = false\n",
			want: true,
		},
		{
			name: "explicit reclaim_on_input = false sticks",
			body: "[terminal.attach]\nreclaim_on_input = false\n",
			want: false,
		},
		{
			name: "explicit reclaim_on_input = true sticks",
			body: "[terminal.attach]\nreclaim_on_input = true\n",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Terminal.Attach.ReclaimOnInput; got != tc.want {
				t.Errorf("Attach.ReclaimOnInput = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTerminalAttachForwardAuthEnvCompat pins the same partial-merge compat trap
// for the forward_auth_env key: an EXISTING [terminal.attach] block written
// BEFORE the key existed must still load with ForwardAuthEnv=true (seeded true in
// Default(); BurntSushi leaves an absent key untouched — the same mechanism
// DefaultOn / RouteProxy / ReclaimOnInput rely on). An explicit
// forward_auth_env = false sticks.
func TestTerminalAttachForwardAuthEnvCompat(t *testing.T) {
	if !Default().Terminal.Attach.ForwardAuthEnv {
		t.Fatal("Default() must seed Attach.ForwardAuthEnv=true")
	}

	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "legacy block without forward_auth_env keeps true",
			body: "[terminal.attach]\nenabled = true\n",
			want: true,
		},
		{
			name: "legacy block with default_on but no forward_auth_env keeps true",
			body: "[terminal.attach]\ndefault_on = false\n",
			want: true,
		},
		{
			name: "explicit forward_auth_env = false sticks",
			body: "[terminal.attach]\nforward_auth_env = false\n",
			want: false,
		},
		{
			name: "explicit forward_auth_env = true sticks",
			body: "[terminal.attach]\nforward_auth_env = true\n",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Terminal.Attach.ForwardAuthEnv; got != tc.want {
				t.Errorf("Attach.ForwardAuthEnv = %v, want %v", got, tc.want)
			}
		})
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
		{"negative idle_timeout", func(c *Config) { c.Terminal.IdleTimeout = "-5m" }, true},
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
