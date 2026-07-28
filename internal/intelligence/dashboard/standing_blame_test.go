package dashboard

import (
	"errors"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termlease"
)

// TestStandingDenialBlameClassification pins WHICH standing-verify outcomes are
// allowed to make a device destroy its saved standing secret.
//
// Forgetting the secret is irreversible for the user — the owner has to mint and
// convey a new one — so only an outcome that actually COMPARED the secret and
// found it wrong may blame it. Before this split, "standing access is currently
// disabled" and "you tried too fast" both surfaced as a credential rejection and
// wiped a perfectly valid secret off the device.
func TestStandingDenialBlameClassification(t *testing.T) {
	const handle = "H-abc"
	const dev = "device-session-raw"

	t.Run("a correct secret verifies", func(t *testing.T) {
		rc, secret, _, _ := newStandingController(t, true, 60)
		ok, _ := rc.VerifyStandingTerminalControlReason(secret, dev, handle)
		if !ok {
			t.Fatal("a correct standing secret was rejected")
		}
		// The legacy bool-only form must agree.
		if !rc.VerifyStandingTerminalControl(secret, dev, handle) {
			t.Fatal("the legacy verifier disagreed with the reasoning verifier")
		}
	})

	t.Run("a wrong secret BLAMES the secret", func(t *testing.T) {
		rc, _, _, _ := newStandingController(t, true, 60)
		_, otherEnc, err := remoteauth.GenerateStandingSecret()
		if err != nil {
			t.Fatalf("GenerateStandingSecret: %v", err)
		}
		ok, denial := rc.VerifyStandingTerminalControlReason(otherEnc, dev, handle)
		if ok {
			t.Fatal("a foreign standing secret was accepted")
		}
		if denial != termlease.StandingDenialBadSecret {
			t.Fatalf("denial = %v, want BadSecret (a JUDGED-and-rejected secret must blame the credential so the device may clear it)", denial)
		}
	})

	t.Run("a malformed secret BLAMES the secret", func(t *testing.T) {
		rc, _, _, _ := newStandingController(t, true, 60)
		ok, denial := rc.VerifyStandingTerminalControlReason("standing.not-base64!!", dev, handle)
		if ok {
			t.Fatal("a malformed standing secret was accepted")
		}
		if denial != termlease.StandingDenialBadSecret {
			t.Fatalf("denial = %v, want BadSecret (an undecodable presented value is a defect of the value itself)", denial)
		}
	})

	t.Run("standing access disabled does NOT blame the secret", func(t *testing.T) {
		rc, secret, _, _ := newStandingController(t, true, 60)
		// The operator toggles standing access off. On the LIVE controller this
		// is byte-identical to a revoke — both arrive as ("", false) — so the
		// only thing that can tell them apart is whether the secret still
		// exists at rest. Here it does (a disable never unlinks it), so the
		// refusal is transient and the device keeps its secret.
		rc.standingAtRest = func() bool { return true }
		rc.ReloadStandingTerminalSecret("", false)
		ok, denial := rc.VerifyStandingTerminalControlReason(secret, dev, handle)
		if ok {
			t.Fatal("standing access is disabled but the verify succeeded — the master gate must still deny")
		}
		if denial != termlease.StandingDenialUnavailable {
			t.Fatalf("denial = %v, want Unavailable (a disabled master gate never compared the secret, and the toggle can flip back)", denial)
		}
	})

	// The A2 split. A genuine revoke DELETES the standing secret at rest, so
	// nothing any device holds can ever match again — the device should stop
	// retrying and clear it. A temporary disable leaves the secret in place and
	// must stay transient (the subtest above). Both look the same in the
	// controller's memory; the at-rest probe is the whole discriminator.
	t.Run("a REVOKED secret (none at rest) is permanent", func(t *testing.T) {
		rc, secret, audit, mu := newStandingController(t, true, 60)
		rc.standingAtRest = func() bool { return false } // revoke unlinked the hash file
		rc.ReloadStandingTerminalSecret("", false)
		ok, denial := rc.VerifyStandingTerminalControlReason(secret, dev, handle)
		if ok {
			t.Fatal("a revoked standing secret verified")
		}
		if denial != termlease.StandingDenialRevoked {
			t.Fatalf("denial = %v, want Revoked (a revoked-and-never-reprovisioned secret is dead forever; leaving it transient makes the device retry it indefinitely)", denial)
		}
		mu.Lock()
		defer mu.Unlock()
		last := (*audit)[len(*audit)-1]
		if last.Detail != "standing_revoked" {
			t.Errorf("audit detail = %q, want standing_revoked (the two states must be distinguishable in the audit trail too)", last.Detail)
		}
	})

	t.Run("an UNKNOWN at-rest state stays transient", func(t *testing.T) {
		// No probe wired (the pre-A2 shape, and the shape any caller that
		// cannot answer must leave). Destroying a secret is irreversible for
		// the user, so "I do not know" must never be reported as permanent.
		rc, secret, _, _ := newStandingController(t, true, 60)
		if rc.standingAtRest != nil {
			t.Fatal("this subtest requires the no-probe shape")
		}
		rc.ReloadStandingTerminalSecret("", false)
		ok, denial := rc.VerifyStandingTerminalControlReason(secret, dev, handle)
		if ok {
			t.Fatal("a disabled gate verified")
		}
		if denial != termlease.StandingDenialUnavailable {
			t.Fatalf("denial = %v, want Unavailable when the at-rest state is unknown", denial)
		}
	})

	t.Run("rate limiting does NOT blame the secret", func(t *testing.T) {
		// A budget of 1/min: the first attempt consumes it, the second is
		// throttled without ever reaching the compare.
		rc, secret, _, _ := newStandingController(t, true, 1)
		if ok, _ := rc.VerifyStandingTerminalControlReason(secret, dev, handle); !ok {
			t.Fatal("the first (budgeted) attempt should verify")
		}
		var sawThrottle bool
		for i := 0; i < 20 && !sawThrottle; i++ {
			ok, denial := rc.VerifyStandingTerminalControlReason(secret, dev, handle)
			if ok {
				continue // still inside the budget
			}
			sawThrottle = true
			if denial != termlease.StandingDenialUnavailable {
				t.Fatalf("denial = %v, want Unavailable (a rate-limited attempt never compared the secret)", denial)
			}
		}
		if !sawThrottle {
			t.Skip("the rate limiter never throttled at 1/min — nothing to assert")
		}
	})
}

// TestAuthorizeStandingSurfacesTheTransientSentinel proves the blame
// classification actually reaches the authorization boundary through the real
// termlease path: a disabled master gate denies with ErrStandingUnavailable
// (which the cmd adapter maps to the transient wire reason), not
// ErrCapabilityRejected (which the device treats as proof its secret is dead).
func TestAuthorizeStandingSurfacesTheTransientSentinel(t *testing.T) {
	const handle = "H-abc"
	rc, secret, _, _ := newStandingController(t, true, 60)
	raw, err := rc.sessions.Create()
	if err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	req := termlease.AuthorizeRequest{
		Handle:          handle,
		DeviceSessionID: raw,
		CapabilityToken: secret,
		RemoteExposed:   true,
		AllowTerminal:   true,
	}

	// Baseline: with standing armed, the conjunction grants.
	g, err := termlease.AuthorizeStanding(req, rc, allowAllLaunchPolicy{}, rc)
	if err != nil || !g.Authorized() || !g.Standing() {
		t.Fatalf("armed standing acquire = (%v, %v)", g.Authorized(), err)
	}

	// Now switch standing access off and retry.
	rc.ReloadStandingTerminalSecret("", false)
	g2, err2 := termlease.AuthorizeStanding(req, rc, allowAllLaunchPolicy{}, rc)
	if g2.Authorized() {
		t.Fatal("a disabled standing gate still authorized — the denial must be absolute")
	}
	if !errors.Is(err2, termlease.ErrStandingUnavailable) {
		t.Fatalf("denial = %v, want ErrStandingUnavailable (a credential rejection here wipes the device's secret)", err2)
	}

	// And the A2 half: with the secret GONE at rest (a real revoke), the same
	// path surfaces the permanent sentinel instead — still absolutely denied.
	rc.standingAtRest = func() bool { return false }
	g3, err3 := termlease.AuthorizeStanding(req, rc, allowAllLaunchPolicy{}, rc)
	if g3.Authorized() {
		t.Fatal("a revoked standing secret authorized")
	}
	if !errors.Is(err3, termlease.ErrStandingRevoked) {
		t.Fatalf("denial = %v, want ErrStandingRevoked", err3)
	}
}
