package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// TestTerminalContinuityDefaults pins the 2026-07-25 mobile terminal-continuity
// defaults and — just as importantly — that every one of them is a KEY an
// operator can tighten back. Each value below widened a real security bound at
// the operator's explicit request, so none of them may become hard-coded.
func TestTerminalContinuityDefaults(t *testing.T) {
	d := Default()

	// Device sessions: 24h idle inside a 48-HOUR absolute cap (the operator's
	// 2026-07-25 decision, replacing the 7d the implementation first proposed).
	if d.Remote.SessionIdleMinutes != 1440 {
		t.Errorf("remote.session_idle_minutes = %d, want 1440 (24h)", d.Remote.SessionIdleMinutes)
	}
	if d.Remote.SessionTTLMinutes != 2880 {
		t.Errorf("remote.session_ttl_minutes = %d, want 2880 (48h)", d.Remote.SessionTTLMinutes)
	}
	// Coherence: an absolute cap at or below the idle bound makes the idle
	// bound unreachable, so the idle target would be a lie.
	if d.Remote.SessionTTLMinutes <= d.Remote.SessionIdleMinutes {
		t.Errorf("session_ttl_minutes (%d) must exceed session_idle_minutes (%d)",
			d.Remote.SessionTTLMinutes, d.Remote.SessionIdleMinutes)
	}

	// Single-use execute capability: long enough for a human round-trip to a
	// mail/chat app. Single-use semantics are unaffected by the duration.
	if d.Remote.CapabilityTTLMinutes != 10 {
		t.Errorf("remote.capability_ttl_minutes = %d, want 10", d.Remote.CapabilityTTLMinutes)
	}

	// Terminal websocket liveness: ping every 30s, 10s per pong, tolerate 5
	// consecutive misses (~3.3 min) so a frozen backgrounded mobile tab is not
	// mistaken for a dead peer.
	if d.Terminal.WSPingIntervalSeconds != 30 {
		t.Errorf("terminal.ws_ping_interval_seconds = %d, want 30", d.Terminal.WSPingIntervalSeconds)
	}
	if d.Terminal.WSPingTimeoutSeconds != 10 {
		t.Errorf("terminal.ws_ping_timeout_seconds = %d, want 10", d.Terminal.WSPingTimeoutSeconds)
	}
	if d.Terminal.WSPingFailuresAllowed != 5 {
		t.Errorf("terminal.ws_ping_failures_allowed = %d, want 5", d.Terminal.WSPingFailuresAllowed)
	}
	// The tolerated outage must be measured in minutes — a sub-minute window is
	// shorter than a trip to another app and would reintroduce the defect.
	graceSeconds := d.Terminal.WSPingFailuresAllowed *
		(d.Terminal.WSPingIntervalSeconds + d.Terminal.WSPingTimeoutSeconds)
	if graceSeconds < 120 {
		t.Errorf("frozen-tab grace = %ds, want >= 120s", graceSeconds)
	}

	if err := Validate(d); err != nil {
		t.Fatalf("Validate(Default()) = %v", err)
	}
}

// TestTerminalContinuityOverridesLoad pins that every widened bound is
// tightenable from config.toml — the non-negotiable condition on widening a
// security duration.
func TestTerminalContinuityOverridesLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[remote]
session_idle_minutes = 30
session_ttl_minutes = 120
capability_ttl_minutes = 1

[terminal]
ws_ping_interval_seconds = 5
ws_ping_timeout_seconds = 2
ws_ping_failures_allowed = 1
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c, err := Load(LoadOptions{GlobalPath: path, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Remote.SessionIdleMinutes != 30 || c.Remote.SessionTTLMinutes != 120 {
		t.Errorf("session bounds = {idle:%d ttl:%d}, want the overrides",
			c.Remote.SessionIdleMinutes, c.Remote.SessionTTLMinutes)
	}
	if c.Remote.CapabilityTTLMinutes != 1 {
		t.Errorf("capability_ttl_minutes = %d, want 1", c.Remote.CapabilityTTLMinutes)
	}
	if c.Terminal.WSPingIntervalSeconds != 5 || c.Terminal.WSPingTimeoutSeconds != 2 ||
		c.Terminal.WSPingFailuresAllowed != 1 {
		t.Errorf("ws ping policy = %d/%d/%d, want the overrides (1 restores one-strike liveness)",
			c.Terminal.WSPingIntervalSeconds, c.Terminal.WSPingTimeoutSeconds, c.Terminal.WSPingFailuresAllowed)
	}
}

// TestTerminalContinuityValidation pins that the new keys reject nonsense
// instead of silently installing a degenerate bound.
func TestTerminalContinuityValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"negative ping interval", func(c *Config) { c.Terminal.WSPingIntervalSeconds = -1 }},
		{"negative ping timeout", func(c *Config) { c.Terminal.WSPingTimeoutSeconds = -1 }},
		{"negative failure budget", func(c *Config) { c.Terminal.WSPingFailuresAllowed = -1 }},
		{"negative capability ttl", func(c *Config) {
			armRemote(c)
			c.Remote.CapabilityTTLMinutes = -1
		}},
		// The two session-bound keys the 2026-07-25 arc widened. Before review
		// B2 neither was validated at all: `session_ttl_minutes = -720` loaded
		// cleanly and NewSessionStore's `<= 0 ⇒ default` clamp silently handed
		// back the LONGEST window the build offers — the inverse of what the
		// operator wrote, and a blast radius that grew with the widening.
		{"negative session ttl", func(c *Config) {
			armRemote(c)
			c.Remote.SessionTTLMinutes = -720
		}},
		{"negative session idle", func(c *Config) {
			armRemote(c)
			c.Remote.SessionIdleMinutes = -1
		}},
		// Coherence: an idle window larger than the absolute cap is safe (the
		// cap wins) but silently inoperative — and that interaction is the
		// stated reason the absolute cap moved at all, so it must be loud.
		{"idle exceeds absolute ttl", func(c *Config) {
			armRemote(c)
			c.Remote.SessionTTLMinutes = 120
			c.Remote.SessionIdleMinutes = 240
		}},
		{"idle exceeds the DEFAULT absolute ttl", func(c *Config) {
			armRemote(c)
			c.Remote.SessionTTLMinutes = 0 // 0 ⇒ the 48h default
			c.Remote.SessionIdleMinutes = DefaultRemoteSessionTTLMinutes + 1
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			if err := Validate(c); err == nil {
				t.Fatal("Validate accepted a negative bound")
			}
		})
	}
}

// armRemote turns the [remote] rail on in a shape that otherwise validates, so
// a table row can isolate the ONE key it is testing (validateRemote's bound
// checks run only when the rail is enabled).
func armRemote(c *Config) {
	c.Remote.Enabled = true
	c.Remote.Mode = "tailscale"
	c.Remote.TailscaleBackendAddr = "127.0.0.1:8099"
	c.Remote.TrustedHosts = []string{"box.ts.net"}
}

// TestSessionBoundZeroMeansDefault pins the OTHER half of the B2 contract: 0 is
// a legal value meaning "use the package default" (the convention
// capability_ttl_minutes already follows), not "never expires" and not an error.
func TestSessionBoundZeroMeansDefault(t *testing.T) {
	c := Default()
	armRemote(&c)
	c.Remote.SessionTTLMinutes = 0
	c.Remote.SessionIdleMinutes = 0
	c.Remote.CapabilityTTLMinutes = 0
	if err := Validate(c); err != nil {
		t.Fatalf("Validate rejected the documented 0 = default spelling: %v", err)
	}
}

// TestConfigSessionDefaultsMatchRemoteauth pins the two spellings of the same
// bound in lock-step. internal/config deliberately does not import the auth
// layer, so the minute-valued defaults are duplicated — this is the guard that
// keeps the duplication honest (a 48h config default paired with a 7d
// remoteauth constant would mean the shipped bound depends on which path
// populated SessionParams).
func TestConfigSessionDefaultsMatchRemoteauth(t *testing.T) {
	d := Default()
	if got, want := time.Duration(d.Remote.SessionTTLMinutes)*time.Minute, remoteauth.DefaultSessionTTL; got != want {
		t.Errorf("config session TTL default = %v, remoteauth.DefaultSessionTTL = %v", got, want)
	}
	if got, want := time.Duration(d.Remote.SessionIdleMinutes)*time.Minute, remoteauth.DefaultSessionIdle; got != want {
		t.Errorf("config session idle default = %v, remoteauth.DefaultSessionIdle = %v", got, want)
	}
	if remoteauth.DefaultSessionTTL != 48*time.Hour {
		t.Errorf("absolute device-session TTL = %v, want 48h (operator decision 2026-07-25)", remoteauth.DefaultSessionTTL)
	}
}
