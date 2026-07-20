package termlease

import (
	"errors"
	"testing"
)

// standingFn adapts a func to StandingVerifier for the tests.
type standingFn func(secret, dev, handle string) bool

func (s standingFn) VerifyStandingTerminalControl(secret, dev, handle string) bool {
	return s(secret, dev, handle)
}

// TestAuthorizeStandingHappyPath: every §4.δ leg holds AND the standing secret
// verifies → an authorized grant bound to handle + device fingerprint.
func TestAuthorizeStandingHappyPath(t *testing.T) {
	verified := ""
	g, err := AuthorizeStanding(
		validReq(),
		okSessions{},
		policyFn(func(string) bool { return true }),
		standingFn(func(secret, dev, handle string) bool { verified = secret; return true }),
	)
	if err != nil {
		t.Fatalf("AuthorizeStanding: %v", err)
	}
	if !g.Authorized() {
		t.Fatal("grant not authorized on standing happy path")
	}
	if g.Handle() != "h1" {
		t.Errorf("grant handle = %q, want h1", g.Handle())
	}
	if g.Holder() == "" {
		t.Error("grant holder (device fingerprint) must be set")
	}
	if verified == "" {
		t.Error("standing verifier was not passed the credential")
	}
	if !g.Standing() {
		t.Error("AuthorizeStanding grant must report Standing()==true (provenance for the takeover hook)")
	}
}

// TestStandingProvenance pins the credential-leg provenance split: only
// AuthorizeStanding mints Standing()==true; the single-use Authorize path and a
// zero-value grant report false.
func TestStandingProvenance(t *testing.T) {
	g, err := Authorize(
		validReq(),
		okSessions{},
		policyFn(func(string) bool { return true }),
		capFn(func(token, confirm, dev, handle string) bool { return true }),
	)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if g.Standing() {
		t.Error("single-use Authorize grant must report Standing()==false")
	}
	if (WriterGrant{}).Standing() {
		t.Error("zero-value grant must report Standing()==false")
	}
}

// TestAuthorizeStandingSecretRejected: a valid conjunction but a failed standing
// verify denies exactly like a rejected capability.
func TestAuthorizeStandingSecretRejected(t *testing.T) {
	_, err := AuthorizeStanding(
		validReq(),
		okSessions{},
		policyFn(func(string) bool { return true }),
		standingFn(func(string, string, string) bool { return false }),
	)
	if !errors.Is(err, ErrCapabilityRejected) {
		t.Fatalf("standing reject err = %v, want ErrCapabilityRejected", err)
	}
}

// TestAuthorizeStandingShortCircuitsBeforeVerify: an earlier failing conjunction
// leg denies WITHOUT ever calling the standing verifier (fail-fast; the verifier
// carries the rate-limit + audit side effects, which must not fire on a
// pre-credential denial).
func TestAuthorizeStandingShortCircuitsBeforeVerify(t *testing.T) {
	cases := []struct {
		name    string
		req     AuthorizeRequest
		sessErr error
		policy  bool
		wantErr error
	}{
		{"not-remote-exposed", func() AuthorizeRequest { r := validReq(); r.RemoteExposed = false; return r }(), nil, true, ErrNotRemoteExposed},
		{"allow-terminal-off", func() AuthorizeRequest { r := validReq(); r.AllowTerminal = false; return r }(), nil, true, ErrTerminalDisabled},
		{"missing-field", func() AuthorizeRequest { r := validReq(); r.Handle = ""; return r }(), nil, true, ErrMissingField},
		{"no-session", validReq(), errors.New("bad"), true, ErrNoDeviceSession},
		{"policy-denied", validReq(), nil, false, ErrPolicyDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			_, err := AuthorizeStanding(
				tc.req,
				okSessions{err: tc.sessErr},
				policyFn(func(string) bool { return tc.policy }),
				standingFn(func(string, string, string) bool { called = true; return true }),
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if called {
				t.Error("standing verifier was called after an earlier leg should have short-circuited")
			}
		})
	}
}
