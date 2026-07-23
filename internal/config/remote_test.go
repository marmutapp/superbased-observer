package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoteDefaultsOff pins the plan §5 posture: the [remote] rail is OFF by
// default in every path (the Default() partial-merge seed), so a fresh install
// is loopback-only and opens no network-facing surface. The secure seeds
// (TLS required, terminal off, bounded rate-limit/sessions) are present so a
// consenting operator inherits them, but nothing exposes until they opt in.
func TestRemoteDefaultsOff(t *testing.T) {
	d := Default()
	if d.Remote.Enabled {
		t.Error("remote.enabled defaulted ON — exposure must be opt-in")
	}
	if d.Remote.Mode != "off" {
		t.Errorf("remote.mode = %q, want off", d.Remote.Mode)
	}
	if !d.Remote.RequireTLS {
		t.Error("remote.require_tls defaulted false — TLS is required for all remote access (§5)")
	}
	if d.Remote.AllowTerminal {
		t.Error("remote.allow_terminal defaulted ON — the execute-tier terminal must be off by default (§4.2)")
	}
	if !d.Remote.AllowRemoteTerminalTakeover {
		t.Error("remote.allow_remote_terminal_takeover defaulted false — authenticated handoff must default on")
	}
	if !d.Remote.AllowTerminalView {
		t.Error("remote.allow_terminal_view defaulted false — read-only view must default on (mirrors the takeover default-true precedent)")
	}
	if d.Remote.Notify.Enabled {
		t.Error("remote.notify.enabled defaulted ON — the outbound rail must be opt-in")
	}
	if d.Remote.RateLimitPerMin <= 0 || d.Remote.MaxSessions <= 0 {
		t.Errorf("remote rate-limit/max-sessions seeds missing: %+v", d.Remote)
	}
}

// TestAllowRemoteTerminalTakeoverExplicitFalseRoundTrips pins the default-true
// partial-merge behavior: an omitted key inherits true, while an explicit false
// survives load and the whole-config writer used by dashboard management.
func TestAllowRemoteTerminalTakeoverExplicitFalseRoundTrips(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input.toml")
	if err := os.WriteFile(in, []byte("[remote]\nallow_remote_terminal_takeover = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: in})
	if err != nil {
		t.Fatalf("Load explicit false: %v", err)
	}
	if cfg.Remote.AllowRemoteTerminalTakeover {
		t.Fatal("explicit false was overwritten by the default-true seed")
	}

	out := filepath.Join(dir, "output.toml")
	if err := WriteToml(out, cfg); err != nil {
		t.Fatalf("WriteToml: %v", err)
	}
	roundTrip, err := Load(LoadOptions{GlobalPath: out})
	if err != nil {
		t.Fatalf("Load round-trip: %v", err)
	}
	if roundTrip.Remote.AllowRemoteTerminalTakeover {
		t.Fatal("explicit false did not survive WriteToml round-trip")
	}
}

// TestAllowTerminalViewDefaultTrueCompat pins the default-true partial-merge
// behavior for the remote-VIEW opt-in (the same compat trap as the takeover
// flag and [terminal.attach].default_on): a legacy [remote] block written
// BEFORE the allow_terminal_view key existed must still load with the field
// true, because Default() seeds it true and BurntSushi's field-level decode
// leaves the absent key untouched. An explicit false must stick; an explicit
// true must stick.
func TestAllowTerminalViewDefaultTrueCompat(t *testing.T) {
	if !Default().Remote.AllowTerminalView {
		t.Fatal("Default() must seed Remote.AllowTerminalView=true")
	}

	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			// The compat trap: a legacy [remote] block with no
			// allow_terminal_view key inherits the default-true seed.
			name: "legacy remote block without allow_terminal_view keeps true",
			body: "[remote]\nallow_terminal = false\n",
			want: true,
		},
		{
			name: "legacy block with takeover key but no view key keeps true",
			body: "[remote]\nallow_remote_terminal_takeover = false\n",
			want: true,
		},
		{
			name: "explicit allow_terminal_view = false sticks",
			body: "[remote]\nallow_terminal_view = false\n",
			want: false,
		},
		{
			name: "explicit allow_terminal_view = true sticks",
			body: "[remote]\nallow_terminal_view = true\n",
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
			if got := cfg.Remote.AllowTerminalView; got != tc.want {
				t.Errorf("Remote.AllowTerminalView = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAllowTerminalViewExplicitFalseRoundTrips pins that an explicit false
// survives Load AND the whole-config WriteToml used by dashboard management,
// mirroring TestAllowRemoteTerminalTakeoverExplicitFalseRoundTrips.
func TestAllowTerminalViewExplicitFalseRoundTrips(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input.toml")
	if err := os.WriteFile(in, []byte("[remote]\nallow_terminal_view = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{GlobalPath: in})
	if err != nil {
		t.Fatalf("Load explicit false: %v", err)
	}
	if cfg.Remote.AllowTerminalView {
		t.Fatal("explicit false was overwritten by the default-true seed")
	}

	out := filepath.Join(dir, "output.toml")
	if err := WriteToml(out, cfg); err != nil {
		t.Fatalf("WriteToml: %v", err)
	}
	roundTrip, err := Load(LoadOptions{GlobalPath: out})
	if err != nil {
		t.Fatalf("Load round-trip: %v", err)
	}
	if roundTrip.Remote.AllowTerminalView {
		t.Fatal("explicit false did not survive WriteToml round-trip")
	}
}

// TestRemoteValidation exercises validateRemote's enum + required-field gates.
func TestRemoteValidation(t *testing.T) {
	base := Default()

	// Phase 2: tailscale mode WITHOUT a loopback backend addr is rejected.
	c := base
	c.Remote.Enabled = true
	c.Remote.Mode = "tailscale"
	c.Remote.TrustedHosts = []string{"host.tailnet.ts.net"}
	if err := Validate(c); err == nil {
		t.Error("remote.mode=tailscale without a backend addr should be rejected")
	}

	// tailscale with a NON-loopback backend addr is rejected (plan §4.4).
	c = base
	c.Remote.Enabled = true
	c.Remote.Mode = "tailscale"
	c.Remote.TrustedHosts = []string{"host.tailnet.ts.net"}
	c.Remote.TailscaleBackendAddr = "0.0.0.0:8890"
	if err := Validate(c); err == nil {
		t.Error("remote.mode=tailscale with a non-loopback backend addr should be rejected")
	}

	// tailscale with a loopback backend addr but NO trusted_hosts is rejected
	// (the Host allow-list has no fallback, plan §4.5).
	c = base
	c.Remote.Enabled = true
	c.Remote.Mode = "tailscale"
	c.Remote.TailscaleBackendAddr = "127.0.0.1:8890"
	if err := Validate(c); err == nil {
		t.Error("remote.mode=tailscale without trusted_hosts should be rejected")
	}

	// tailscale fully armed (loopback backend + tailnet host) passes.
	c = base
	c.Remote.Enabled = true
	c.Remote.Mode = "tailscale"
	c.Remote.TailscaleBackendAddr = "127.0.0.1:8890"
	c.Remote.TrustedHosts = []string{"host.tailnet.ts.net"}
	if err := Validate(c); err != nil {
		t.Errorf("fully-armed tailscale config rejected: %v", err)
	}

	// lan mode is deferred (Phase 3) and rejected.
	c = base
	c.Remote.Enabled = true
	c.Remote.Mode = "lan"
	c.Remote.BindAddr = "192.168.1.10:8080"
	if err := Validate(c); err == nil {
		t.Error("remote.mode=lan should be rejected (Phase 3 deferred)")
	}

	// A bogus mode is rejected.
	c = base
	c.Remote.Enabled = true
	c.Remote.Mode = "bananas"
	if err := Validate(c); err == nil {
		t.Error("bogus remote.mode should be rejected")
	}

	// notify enabled without a URL is rejected.
	c = base
	c.Remote.Notify.Enabled = true
	c.Remote.Notify.Kind = "webhook"
	c.Remote.Notify.URL = ""
	if err := Validate(c); err == nil {
		t.Error("remote.notify.enabled without url should be rejected")
	}

	// notify enabled with a valid https URL passes even with the rail off.
	c = base
	c.Remote.Notify.Enabled = true
	c.Remote.Notify.URL = "https://ntfy.sh/mytopic"
	if err := Validate(c); err != nil {
		t.Errorf("valid notify config rejected: %v", err)
	}

	// Default (off) always validates.
	if err := Validate(base); err != nil {
		t.Errorf("default config failed validation: %v", err)
	}
}
