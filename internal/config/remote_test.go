package config

import "testing"

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
	if d.Remote.Notify.Enabled {
		t.Error("remote.notify.enabled defaulted ON — the outbound rail must be opt-in")
	}
	if d.Remote.RateLimitPerMin <= 0 || d.Remote.MaxSessions <= 0 {
		t.Errorf("remote rate-limit/max-sessions seeds missing: %+v", d.Remote)
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
