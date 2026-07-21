package termlease

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// TestHolderKeyOfMatchesRemoteauthHashSessionID pins holderKeyOf byte-identical
// to remoteauth.HashSessionID for every valid (non-empty) device-session id.
// grant.go's doc comment on holderKeyOf claims this identity so that a
// per-device revoke — which resolves a device to its full session hash and
// matches a writer-lease HolderKey against it — targets exactly the leases
// belonging to that device. If the two functions ever diverge for a real id,
// a revoke silently stops matching the leases it was supposed to close: this
// test exists so that divergence is a loud, immediate CI failure rather than a
// live under-revoke.
//
// remoteauth.HashSessionID IS exported (internal/remoteauth/session.go), so
// the test calls it directly rather than reimplementing sha256-hex here.
func TestHolderKeyOfMatchesRemoteauthHashSessionID(t *testing.T) {
	longID := strings.Repeat("a-very-long-device-session-fragment-", 50) + "tail"

	cases := []struct {
		name string
		id   string
	}{
		{
			name: "typical 32-byte random hex (crypto/rand device session shape)",
			id:   randomHex32(t),
		},
		{
			name: "short ASCII",
			id:   "abc",
		},
		{
			name: "UTF-8 multibyte",
			id:   "デバイス-session-🔑-émoji",
		},
		{
			name: "long string",
			id:   longID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.id == "" {
				t.Fatalf("test case must use a non-empty id — the empty case is covered separately")
			}
			got := holderKeyOf(tc.id)
			want := remoteauth.HashSessionID(tc.id)
			if got != want {
				t.Errorf("holderKeyOf(%q) = %q, want %q (remoteauth.HashSessionID) — grant.go's holderKeyOf has diverged from remoteauth.HashSessionID; a per-device revoke will silently under-revoke", tc.id, got, want)
			}
			// Sanity: both sides actually produced a real 64-char sha256-hex
			// digest, not two empty strings that happened to be equal.
			if len(got) != 64 {
				t.Fatalf("holderKeyOf(%q) = %q, want a 64-char sha256-hex digest", tc.id, got)
			}
		})
	}
}

// TestHolderKeyOfEmptyInputDivergesFromRemoteauthHashSessionID documents and
// pins the ONE known, intentional divergence between holderKeyOf and
// remoteauth.HashSessionID: holderKeyOf("") short-circuits to "" (grant.go's
// explicit empty guard, mirrored by fingerprint's own empty guard), while
// remoteauth.HashSessionID("") returns the real sha256-hex digest of the empty
// byte string. This is unreachable in practice — checkConjunctionPrefix
// rejects an empty DeviceSessionID with ErrMissingField before Authorize ever
// reaches holderKeyOf — so the divergence is safe. Pinned here (rather than
// folded into the table above) so a change to either empty-input behavior is
// loud instead of silently drifting.
func TestHolderKeyOfEmptyInputDivergesFromRemoteauthHashSessionID(t *testing.T) {
	got := holderKeyOf("")
	if got != "" {
		t.Fatalf("holderKeyOf(\"\") = %q, want \"\" (the documented empty-input guard) — if this changed intentionally, update this test and re-check the divergence note on holderKeyOf/grant.go", got)
	}
	hashOfEmpty := remoteauth.HashSessionID("")
	if hashOfEmpty == "" {
		t.Fatalf("remoteauth.HashSessionID(\"\") returned empty — expected the real sha256-hex digest of the empty string, which would mean holderKeyOf and HashSessionID no longer diverge on empty input and the intentional-divergence note is stale")
	}
	if got == hashOfEmpty {
		t.Fatalf("holderKeyOf(\"\") unexpectedly equals remoteauth.HashSessionID(\"\") (%q) — the documented empty-input divergence has disappeared; update the divergence note on holderKeyOf/grant.go", hashOfEmpty)
	}
}

// randomHex32 generates a 64-char hex string shaped like a real crypto/rand
// device-session id (the format Authorize/AuthorizeStanding actually see in
// production, per grant.go's doc comments on DeviceSessionID).
func randomHex32(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(buf)
}
