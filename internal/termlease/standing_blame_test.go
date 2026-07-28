package termlease

import (
	"errors"
	"testing"
)

// blameSess/blamePolicy satisfy the non-credential legs so the tables below
// isolate the standing credential leg.
type blameSess struct{}

func (blameSess) Validate(string) error { return nil }

type blamePolicy struct{}

func (blamePolicy) Allowed(string) bool { return true }

// plainVerifier implements ONLY the legacy StandingVerifier — it must keep the
// pre-2026-07-25 behaviour (every denial reported as a credential rejection).
type plainVerifier struct{ ok bool }

func (v plainVerifier) VerifyStandingTerminalControl(string, string, string) bool { return v.ok }

// reasoningVerifier implements the additive ReasoningStandingVerifier.
type reasoningVerifier struct {
	ok     bool
	denial StandingDenial
}

func (v reasoningVerifier) VerifyStandingTerminalControl(string, string, string) bool { return v.ok }

func (v reasoningVerifier) VerifyStandingTerminalControlReason(string, string, string) (bool, StandingDenial) {
	return v.ok, v.denial
}

// TestAuthorizeStandingDenialBlame pins the split between a denial that JUDGED
// the presented standing secret (ErrCapabilityRejected — the device may discard
// it) and one that refused without judging it (ErrStandingUnavailable — the
// device MUST keep it). Both deny; only the downstream blame differs. Before the
// split, a momentary rate-limit or a disabled-then-re-enabled standing toggle
// made the device delete a perfectly good secret and forced a re-mint.
func TestAuthorizeStandingDenialBlame(t *testing.T) {
	req := AuthorizeRequest{
		Handle:          "h1",
		DeviceSessionID: "dev1",
		CapabilityToken: "standing.whatever",
		RemoteExposed:   true,
		AllowTerminal:   true,
	}

	tests := []struct {
		name        string
		verifier    StandingVerifier
		wantGranted bool
		wantErr     error
	}{
		{
			name:        "legacy verifier, success",
			verifier:    plainVerifier{ok: true},
			wantGranted: true,
		},
		{
			name:     "legacy verifier, denial still blames the secret",
			verifier: plainVerifier{ok: false},
			wantErr:  ErrCapabilityRejected,
		},
		{
			name:        "reasoning verifier, success",
			verifier:    reasoningVerifier{ok: true},
			wantGranted: true,
		},
		{
			name:     "reasoning verifier, judged and rejected",
			verifier: reasoningVerifier{denial: StandingDenialBadSecret},
			wantErr:  ErrCapabilityRejected,
		},
		{
			name:     "reasoning verifier, refused without judging",
			verifier: reasoningVerifier{denial: StandingDenialUnavailable},
			wantErr:  ErrStandingUnavailable,
		},
		{
			// The A2 third outcome: the server holds no standing secret at all
			// (revoked, never re-provisioned). Nothing was compared, but nothing
			// can ever match again either, so this is PERMANENT — the device may
			// clear its saved copy, exactly as for a judged rejection.
			name:     "reasoning verifier, revoked and never re-provisioned",
			verifier: reasoningVerifier{denial: StandingDenialRevoked},
			wantErr:  ErrStandingRevoked,
		},
		{
			// Fail-safe: a classification this build does not know must land on
			// the PRESERVING side. Clearing a secret is irreversible for the
			// user; one more denied attempt is not.
			name:     "reasoning verifier, unrecognised classification",
			verifier: reasoningVerifier{denial: StandingDenial(200)},
			wantErr:  ErrStandingUnavailable,
		},
		{
			// A verifier that denies but reports None (the zero value) is
			// malformed — it must still deny, and still on the preserving side.
			name:     "reasoning verifier, denial with a None classification",
			verifier: reasoningVerifier{denial: StandingDenialNone},
			wantErr:  ErrStandingUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, err := AuthorizeStanding(req, blameSess{}, blamePolicy{}, tc.verifier)
			if tc.wantGranted {
				if err != nil {
					t.Fatalf("AuthorizeStanding = %v, want a grant", err)
				}
				if !g.Authorized() || !g.Standing() {
					t.Fatalf("grant = {authorized:%v standing:%v}, want both true", g.Authorized(), g.Standing())
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AuthorizeStanding = %v, want %v", err, tc.wantErr)
			}
			// EVERY denial is equally hard: no grant escapes, whatever the blame.
			if g.Authorized() {
				t.Fatal("a denied AuthorizeStanding returned an AUTHORIZED grant")
			}
		})
	}
}

// TestStandingBlameNeverBypassesTheConjunction pins that the blame split is
// downstream of the §4.δ conjunction and cannot be used to skip any leg: a
// verifier that would report "not blamed" still never runs when an earlier leg
// fails, and the earlier leg's sentinel is what surfaces.
func TestStandingBlameNeverBypassesTheConjunction(t *testing.T) {
	base := AuthorizeRequest{
		Handle:          "h1",
		DeviceSessionID: "dev1",
		CapabilityToken: "standing.whatever",
		RemoteExposed:   true,
		AllowTerminal:   true,
	}
	// A verifier that ALWAYS succeeds — so any denial below must come from an
	// earlier conjunction leg, not the credential leg.
	always := reasoningVerifier{ok: true, denial: StandingDenialNone}

	tests := []struct {
		name    string
		mutate  func(r *AuthorizeRequest)
		wantErr error
	}{
		{"not remote-exposed", func(r *AuthorizeRequest) { r.RemoteExposed = false }, ErrNotRemoteExposed},
		{"allow_terminal off", func(r *AuthorizeRequest) { r.AllowTerminal = false }, ErrTerminalDisabled},
		{"missing handle", func(r *AuthorizeRequest) { r.Handle = "" }, ErrMissingField},
		{"missing device session", func(r *AuthorizeRequest) { r.DeviceSessionID = "" }, ErrMissingField},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			g, err := AuthorizeStanding(req, blameSess{}, blamePolicy{}, always)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AuthorizeStanding = %v, want %v", err, tc.wantErr)
			}
			if g.Authorized() {
				t.Fatal("conjunction leg failed but an authorized grant was returned")
			}
		})
	}
}
