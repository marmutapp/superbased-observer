package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLaunchAllowInstallDefault pins that the guided-install kill-switch is ON
// by default (consent is the dashboard click, so the config only exists to turn
// the affordance OFF), and that a fresh install with no [terminal.launch] block
// still gets AllowInstall=true.
func TestLaunchAllowInstallDefault(t *testing.T) {
	if !Default().Terminal.Launch.AllowInstall {
		t.Fatal("Default() must seed Terminal.Launch.AllowInstall=true")
	}
	// The fresh-launch privilege fields stay OFF.
	if Default().Terminal.Launch.AllowFreshAgent {
		t.Error("AllowFreshAgent must stay default OFF")
	}
}

// TestLaunchAllowInstallPartialMerge pins the compat trap: an existing
// [terminal.launch] block written BEFORE allow_install existed must still load
// with AllowInstall=true (seeded true in Default(); BurntSushi leaves the absent
// key untouched — the same mechanism [terminal.attach].default_on relies on).
// An explicit allow_install = false must stick.
func TestLaunchAllowInstallPartialMerge(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "legacy block without allow_install keeps true",
			body: "[terminal.launch]\nallow_fresh_agent = true\n",
			want: true,
		},
		{
			name: "explicit allow_install = false sticks",
			body: "[terminal.launch]\nallow_install = false\n",
			want: false,
		},
		{
			name: "explicit allow_install = true sticks",
			body: "[terminal.launch]\nallow_install = true\n",
			want: true,
		},
		{
			name: "no terminal block at all keeps true",
			body: "[terminal]\nenabled = true\n",
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
			if got := cfg.Terminal.Launch.AllowInstall; got != tc.want {
				t.Errorf("AllowInstall = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLaunchToolsRoundTrip pins the [launch.tools.<tool>].path override map: a
// keyed entry decodes into the LaunchConfig map, and an absent [launch] block
// leaves the map nil (resolve-normally, the common case).
func TestLaunchToolsRoundTrip(t *testing.T) {
	// Default carries no overrides.
	if Default().Launch.Tools != nil {
		t.Error("Default().Launch.Tools should be nil (no overrides)")
	}

	body := `
[launch.tools.opencode]
path = "/home/dev/.hermes/node/bin/opencode"

[launch.tools.claude-code]
path = "/usr/local/bin/claude"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Launch.Tools["opencode"].Path; got != "/home/dev/.hermes/node/bin/opencode" {
		t.Errorf("opencode path = %q", got)
	}
	if got := cfg.Launch.Tools["claude-code"].Path; got != "/usr/local/bin/claude" {
		t.Errorf("claude-code path = %q", got)
	}
}
