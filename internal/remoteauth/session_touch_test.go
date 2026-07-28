package remoteauth

import (
	"testing"
	"time"
)

// TestSessionDefaultsAreTheWidenedBounds pins the 2026-07-25 mobile
// terminal-continuity defaults: 24h idle inside a 48-HOUR absolute cap (the
// operator's decision, replacing the 7d the implementation first proposed). The
// ordering matters as much as the values — an absolute cap BELOW the idle bound
// makes the idle bound unreachable, which is exactly the incoherence a 12h
// absolute + 24h idle pair would have produced; 48h clears one full idle window
// plus a day of headroom at a quarter of the 7d exposure.
func TestSessionDefaultsAreTheWidenedBounds(t *testing.T) {
	if DefaultSessionIdle != 24*time.Hour {
		t.Fatalf("DefaultSessionIdle = %v, want 24h", DefaultSessionIdle)
	}
	if DefaultSessionTTL != 48*time.Hour {
		t.Fatalf("DefaultSessionTTL = %v, want 48h", DefaultSessionTTL)
	}
	if DefaultSessionTTL <= DefaultSessionIdle {
		t.Fatal("the absolute TTL must exceed the idle bound or the idle bound can never be reached")
	}
	// A zero-valued SessionParams adopts them.
	s := NewSessionStore(SessionParams{})
	if s.params.TTL != DefaultSessionTTL || s.params.Idle != DefaultSessionIdle {
		t.Fatalf("zero params = {TTL:%v Idle:%v}, want the package defaults", s.params.TTL, s.params.Idle)
	}
	// And an explicit override still wins (the bounds stay tightenable).
	s2 := NewSessionStore(SessionParams{TTL: 2 * time.Hour, Idle: 15 * time.Minute})
	if s2.params.TTL != 2*time.Hour || s2.params.Idle != 15*time.Minute {
		t.Fatalf("explicit params = {TTL:%v Idle:%v}, want the overrides", s2.params.TTL, s2.params.Idle)
	}
}

// TestTouchAttachedViewerExtendsWhileSessionLifetimeDoesNot is the core pin for
// the viewer-heartbeat half of the arc. Two contracts must hold AT ONCE:
//
//   - SessionLifetime stays READ-ONLY. Callers that bind a privileged viewer to
//     a session's lifetime depend on it never extending; changing that function
//     for everyone would silently give every bound viewer keep-alive powers.
//   - TouchAttachedViewer, the explicit opt-in, DOES extend — so a user watching
//     a terminal does not have the session expire underneath them.
func TestTouchAttachedViewerExtendsWhileSessionLifetimeDoesNot(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()

	newStore := func(now *time.Time) *SessionStore {
		return NewSessionStore(SessionParams{
			TTL:  24 * time.Hour,
			Idle: 10 * time.Minute,
			Now:  func() time.Time { return *now },
		})
	}

	t.Run("SessionLifetime does not refresh the idle clock", func(t *testing.T) {
		now := base
		s := newStore(&now)
		raw, err := s.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Poll the lifetime repeatedly across the idle window — the read-only
		// accessor must NOT keep the session alive.
		for i := 1; i <= 9; i++ {
			now = base.Add(time.Duration(i) * time.Minute)
			if _, _, live := s.SessionLifetime(raw); !live {
				t.Fatalf("session died early at +%dm", i)
			}
		}
		now = base.Add(11 * time.Minute)
		if _, _, live := s.SessionLifetime(raw); live {
			t.Fatal("SessionLifetime kept the session alive — it must stay read-only")
		}
	})

	t.Run("TouchAttachedViewer refreshes the idle clock", func(t *testing.T) {
		now := base
		s := newStore(&now)
		raw, err := s.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// A viewer attached for an hour, touching every 5 minutes: the session
		// survives well past the 10-minute idle window.
		for i := 1; i <= 12; i++ {
			now = base.Add(time.Duration(i) * 5 * time.Minute)
			if !s.TouchAttachedViewer(raw) {
				t.Fatalf("attached viewer touch failed at +%dm — session expired under a live watcher", i*5)
			}
		}
		if err := s.Validate(raw); err != nil {
			t.Fatalf("Validate after an hour of watching = %v, want nil", err)
		}
	})

	t.Run("TouchAttachedViewer cannot resurrect an expired session", func(t *testing.T) {
		now := base
		s := newStore(&now)
		raw, err := s.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		now = base.Add(11 * time.Minute) // past idle with no touches
		if s.TouchAttachedViewer(raw) {
			t.Fatal("touch reported an EXPIRED session live — it must only extend a live one")
		}
		if err := s.Validate(raw); err == nil {
			t.Fatal("expired session still validates after a touch")
		}
	})

	t.Run("TouchAttachedViewer cannot outlive the absolute TTL", func(t *testing.T) {
		now := base
		s := NewSessionStore(SessionParams{
			TTL:  30 * time.Minute,
			Idle: 10 * time.Minute,
			Now:  func() time.Time { return now },
		})
		raw, err := s.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		for i := 1; i <= 5; i++ { // touch every 5m, i.e. never idle out
			now = base.Add(time.Duration(i) * 5 * time.Minute)
			if !s.TouchAttachedViewer(raw) {
				t.Fatalf("touch failed at +%dm (inside the absolute TTL)", i*5)
			}
		}
		now = base.Add(31 * time.Minute)
		if s.TouchAttachedViewer(raw) {
			t.Fatal("a continuously-watched session survived its ABSOLUTE TTL — the hard bound must still fire")
		}
	})

	t.Run("TouchAttachedViewer cannot revive a revoked session", func(t *testing.T) {
		now := base
		s := newStore(&now)
		raw, err := s.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Revoke(raw); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if s.TouchAttachedViewer(raw) {
			t.Fatal("touch reported a REVOKED session live")
		}
	})

	t.Run("TouchAttachedViewer cannot cross a rotate", func(t *testing.T) {
		now := base
		s := newStore(&now)
		raw, err := s.Create()
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Rotate(); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if s.TouchAttachedViewer(raw) {
			t.Fatal("touch survived a generation rotate")
		}
	})
}

// TestCapabilityTTLHonouredFromConfigAndStillSingleUse pins BOTH halves of the
// capability change: the lifetime is now injectable (so an operator can tighten
// it back below the widened 10-minute default) and the single-use property —
// the actual security control — is untouched.
func TestCapabilityTTLHonouredFromConfigAndStillSingleUse(t *testing.T) {
	if DefaultCapabilityTTL != 10*time.Minute {
		t.Fatalf("DefaultCapabilityTTL = %v, want 10m", DefaultCapabilityTTL)
	}

	base := time.Unix(1_700_000_000, 0).UTC()

	t.Run("default TTL survives a human round-trip to an email client", func(t *testing.T) {
		now := base
		c := NewCapabilityStore(0, func() time.Time { return now }) // 0 ⇒ default
		tok, confirm, err := c.MintTerminalControl("dev", "handle")
		if err != nil {
			t.Fatalf("MintTerminalControl: %v", err)
		}
		now = base.Add(9 * time.Minute) // the old 2m window would have expired
		if !c.ConsumeTerminalControl(tok, confirm, "dev", "handle") {
			t.Fatal("capability expired inside the 10m default window")
		}
	})

	t.Run("configured TTL is honoured", func(t *testing.T) {
		now := base
		c := NewCapabilityStore(2*time.Minute, func() time.Time { return now })
		tok, confirm, err := c.MintTerminalControl("dev", "handle")
		if err != nil {
			t.Fatalf("MintTerminalControl: %v", err)
		}
		now = base.Add(3 * time.Minute)
		if c.ConsumeTerminalControl(tok, confirm, "dev", "handle") {
			t.Fatal("a 2m-configured capability was accepted at +3m — the config TTL is not honoured")
		}
	})

	t.Run("still single-use", func(t *testing.T) {
		now := base
		c := NewCapabilityStore(0, func() time.Time { return now })
		tok, confirm, err := c.MintTerminalControl("dev", "handle")
		if err != nil {
			t.Fatalf("MintTerminalControl: %v", err)
		}
		if !c.ConsumeTerminalControl(tok, confirm, "dev", "handle") {
			t.Fatal("first consume rejected")
		}
		if c.ConsumeTerminalControl(tok, confirm, "dev", "handle") {
			t.Fatal("REPLAY accepted — the widened TTL must not have weakened single-use")
		}
	})

	t.Run("still handle-bound and confirm-bound", func(t *testing.T) {
		now := base
		c := NewCapabilityStore(0, func() time.Time { return now })
		tok, confirm, err := c.MintTerminalControl("dev", "handle-a")
		if err != nil {
			t.Fatalf("MintTerminalControl: %v", err)
		}
		// A wrong confirm consumes NOTHING (§4.γ.2) — the real one still works.
		if c.ConsumeTerminalControl(tok, "wrong-confirm", "dev", "handle-a") {
			t.Fatal("wrong confirm accepted")
		}
		if !c.ConsumeTerminalControl(tok, confirm, "dev", "handle-a") {
			t.Fatal("a failed confirm burned the capability")
		}
		// A capability minted for one handle is refused against another (and is
		// burned by that attempt, since the confirm matched — the documented
		// burn-on-confirmed-hit rule).
		tok2, confirm2, err := c.MintTerminalControl("dev", "handle-a")
		if err != nil {
			t.Fatalf("MintTerminalControl: %v", err)
		}
		if c.ConsumeTerminalControl(tok2, confirm2, "dev", "handle-b") {
			t.Fatal("capability replayed against a DIFFERENT terminal handle")
		}
		// And against a different device session.
		tok3, confirm3, err := c.MintTerminalControl("dev", "handle-a")
		if err != nil {
			t.Fatalf("MintTerminalControl: %v", err)
		}
		if c.ConsumeTerminalControl(tok3, confirm3, "other-dev", "handle-a") {
			t.Fatal("capability replayed against a DIFFERENT device session")
		}
	})
}
