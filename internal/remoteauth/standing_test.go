package remoteauth

import (
	"strings"
	"testing"
)

// TestStandingSecretRoundTrip: generate → prefix present → decode back to the
// same raw bytes → argon2id hash verifies.
func TestStandingSecretRoundTrip(t *testing.T) {
	raw, enc, err := GenerateStandingSecret()
	if err != nil {
		t.Fatalf("GenerateStandingSecret: %v", err)
	}
	if len(raw) != StandingSecretBytes {
		t.Fatalf("raw len = %d, want %d", len(raw), StandingSecretBytes)
	}
	if !strings.HasPrefix(enc, StandingSecretPrefix) {
		t.Fatalf("encoded %q missing prefix %q", enc, StandingSecretPrefix)
	}
	if !IsStandingSecret(enc) {
		t.Fatal("IsStandingSecret false for a freshly generated standing secret")
	}
	got, err := DecodeStandingSecret(enc)
	if err != nil {
		t.Fatalf("DecodeStandingSecret: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatal("decoded standing secret != generated raw")
	}
	hash, err := HashSecret(raw)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if !VerifySecret(hash, got) {
		t.Fatal("VerifySecret false for the round-tripped standing secret")
	}
	// A different secret must NOT verify.
	other, _, _ := GenerateStandingSecret()
	if VerifySecret(hash, other) {
		t.Fatal("VerifySecret true for a DIFFERENT standing secret")
	}
}

// TestOneTimeTokenIsNotStanding: a plain base64url capability-style token never
// looks like a standing secret, so the acquire boundary never misroutes it.
func TestOneTimeTokenIsNotStanding(t *testing.T) {
	tok, err := randToken(32)
	if err != nil {
		t.Fatalf("randToken: %v", err)
	}
	if IsStandingSecret(tok) {
		t.Fatalf("a base64url token %q was misidentified as a standing secret", tok)
	}
	// The prefix ends in '.', outside the base64url alphabet, so no base64url
	// token can ever start with it — the routing branch is collision-free.
	if !strings.HasSuffix(StandingSecretPrefix, ".") {
		t.Fatal("StandingSecretPrefix must end in '.' to stay outside base64url")
	}
}

// TestDecodeStandingSecretRejectsMalformed: missing prefix, bad base64, and
// wrong length all fail closed (never a partial secret).
func TestDecodeStandingSecretRejectsMalformed(t *testing.T) {
	_, encGood, _ := GenerateStandingSecret()
	body := strings.TrimPrefix(encGood, StandingSecretPrefix)

	cases := []string{
		body,                                // no prefix
		StandingSecretPrefix + "!!!not-b64", // bad base64
		StandingSecretPrefix + "YWJj",       // valid base64 but wrong length (3 bytes)
		"",                                  // empty
		StandingSecretPrefix,                // prefix only
	}
	for _, c := range cases {
		if _, err := DecodeStandingSecret(c); err == nil {
			t.Errorf("DecodeStandingSecret(%q) succeeded, want error", c)
		}
	}
	// The good one still decodes.
	if _, err := DecodeStandingSecret(encGood); err != nil {
		t.Fatalf("DecodeStandingSecret(good) = %v", err)
	}
}
