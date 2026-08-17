package orgcontract

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// newSignedResource builds and signs a valid SignedPolicyResource for tests.
func newSignedResource(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey, body string, version int64, caps []string) SignedPolicyResource {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	r := SignedPolicyResource{
		ID:                   "default",
		Version:              version,
		Family:               "admission.input",
		CompilerVersion:      "v1",
		Body:                 body,
		BodyHash:             hex.EncodeToString(sum[:]),
		RequiredCapabilities: caps,
		SelectorsJSON:        "{}",
		PublicKey:            base64.RawURLEncoding.EncodeToString(pub),
		SignedAt:             "2026-08-12T00:00:00Z",
	}
	sig, err := SignPolicyResource(priv, r)
	if err != nil {
		t.Fatalf("SignPolicyResource: %v", err)
	}
	r.Signature = sig
	return r
}

func TestPolicyResourceSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	r := newSignedResource(t, priv, pub, `{"mode":"enforce"}`, 3, []string{"judge", "judge"})
	gotPub, err := VerifyPolicyResource(r)
	if err != nil {
		t.Fatalf("VerifyPolicyResource: %v", err)
	}
	if !gotPub.Equal(pub) {
		t.Fatal("VerifyPolicyResource must return the embedded public key")
	}
}

func TestPolicyResourceSignVerify_RejectionClasses(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	good := newSignedResource(t, priv, pub, `{"mode":"enforce"}`, 3, []string{"judge"})

	cases := []struct {
		name    string
		mutate  func(r *SignedPolicyResource)
		wantErr string
	}{
		{"valid round trip", func(*SignedPolicyResource) {}, ""},
		{"tampered body without recomputed hash", func(r *SignedPolicyResource) { r.Body = `{"mode":"observe"}` }, "body hash mismatch"},
		{"tampered version", func(r *SignedPolicyResource) { r.Version = 4 }, "signature verification failed"},
		{"tampered family", func(r *SignedPolicyResource) { r.Family = "egress.routing_guardrail" }, "signature verification failed"},
		{"tampered capabilities", func(r *SignedPolicyResource) { r.RequiredCapabilities = []string{"other"} }, "signature verification failed"},
		{"tampered selectors", func(r *SignedPolicyResource) { r.SelectorsJSON = `{"team":"x"}` }, "signature verification failed"},
		{"wrong key", func(r *SignedPolicyResource) {
			r.PublicKey = base64.RawURLEncoding.EncodeToString(otherPub)
		}, "signature verification failed"},
		{"malformed public key encoding", func(r *SignedPolicyResource) { r.PublicKey = "!!!" }, "decode public key"},
		{"truncated public key", func(r *SignedPolicyResource) { r.PublicKey = "cHVi" }, "public key is 3 bytes"},
		{"malformed signature encoding", func(r *SignedPolicyResource) { r.Signature = "!!!" }, "decode signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mutate(&r)
			gotPub, err := VerifyPolicyResource(r)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verify: %v", err)
				}
				if !gotPub.Equal(pub) {
					t.Fatal("verify must return the embedded public key")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestPolicyResourceSigning_DomainSeparation proves a signature minted on
// the guard PolicyBundle rail — using the SAME key — cannot verify as a
// policy resource, even when the comparable content (version, body bytes)
// would otherwise line up. This is the "domain separation" mutation proof
// the plan requires (Phase P0 item 4).
func TestPolicyResourceSigning_DomainSeparation(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	const body = `{"mode":"enforce"}`
	const version = int64(3)

	// A signature minted for the guard bundle rail over comparable content.
	bundleSig := SignPolicyBundle(priv, version, []byte(body))

	sum := sha256.Sum256([]byte(body))
	r := SignedPolicyResource{
		ID:              "default",
		Version:         version,
		Family:          "admission.input",
		CompilerVersion: "v1",
		Body:            body,
		BodyHash:        hex.EncodeToString(sum[:]),
		SelectorsJSON:   "{}",
		Signature:       bundleSig,
		PublicKey:       base64.RawURLEncoding.EncodeToString(pub),
		SignedAt:        "2026-08-12T00:00:00Z",
	}
	if _, err := VerifyPolicyResource(r); err == nil {
		t.Fatal("a guard-bundle signature must NOT verify as a policy resource — domain separation broken")
	}

	// And the reverse: a policy-resource signature must not verify as a
	// guard bundle either.
	resourceSig, err := SignPolicyResource(priv, r)
	if err != nil {
		t.Fatalf("SignPolicyResource: %v", err)
	}
	bundle := PolicyBundle{
		Version:    version,
		BundleTOML: body,
		Signature:  resourceSig,
		PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
	}
	if _, err := VerifyPolicyBundle(bundle); err == nil {
		t.Fatal("a policy-resource signature must NOT verify as a guard bundle — domain separation broken")
	}
}

// TestNormalizeCapabilities_RejectsNewlineAndBadGrammar pins the caps
// grammar mutation proof (Phase P0 item 4): a capability containing a
// newline (or otherwise violating ^[a-z][a-z0-9_.]{0,63}$) is rejected,
// never silently included in — or splitting — the signed message.
func TestNormalizeCapabilities_RejectsNewlineAndBadGrammar(t *testing.T) {
	badCases := []string{
		"judge\nother",          // embedded newline — the framing-hostile case
		"Judge",                 // uppercase
		"1judge",                // must start with a letter
		"judge caps",            // space
		"",                      // empty token
		strings.Repeat("a", 65), // over the 64-char cap
	}
	for _, c := range badCases {
		t.Run(c, func(t *testing.T) {
			if _, err := NormalizeCapabilities([]string{c}); err == nil {
				t.Fatalf("NormalizeCapabilities(%q) = nil error, want a grammar rejection", c)
			}
		})
	}

	// A capability list containing a bad token must also fail inside the
	// signing message construction, not just the standalone helper.
	if _, err := PolicyResourceSigningMessage("default", 1, "admission.input", "v1", "deadbeef", "{}", []string{"judge\nx"}); err == nil {
		t.Fatal("PolicyResourceSigningMessage must reject a newline-bearing capability")
	}
}

func TestNormalizeCapabilities_SortsDedupesAndCaps(t *testing.T) {
	got, err := NormalizeCapabilities([]string{"zeta", "alpha", "zeta", "beta"})
	if err != nil {
		t.Fatalf("NormalizeCapabilities: %v", err)
	}
	want := []string{"alpha", "beta", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// Over the count cap.
	many := make([]string, MaxPolicyResourceCapabilities+1)
	for i := range many {
		many[i] = "cap" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	if _, err := NormalizeCapabilities(many); err == nil {
		t.Fatal("expected an error for exceeding the capability count cap")
	}
}

// TestVerifyPolicyResource_BodyHashMismatch is the explicit BodyHash
// mutation proof the plan requires (Phase P0 item 4).
func TestVerifyPolicyResource_BodyHashMismatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	r := newSignedResource(t, priv, pub, `{"mode":"enforce"}`, 1, nil)
	r.BodyHash = strings.Repeat("0", 64) // valid-shaped hex, but wrong value
	if _, err := VerifyPolicyResource(r); err == nil || !strings.Contains(err.Error(), "body hash mismatch") {
		t.Fatalf("err = %v, want body hash mismatch", err)
	}
}

// TestPolicyResourceMessageDigest_EqualVersionIdentity pins the equal-
// version replay primitive (plan §4.2/§6.3): the digest changes with ANY
// field, and a same-version republish that only changes capabilities or
// selectors changes the digest too (so it is caught by the agent's
// equal-floor digest check, not just a changed Body).
func TestPolicyResourceMessageDigest_EqualVersionIdentity(t *testing.T) {
	base := SignedPolicyResource{
		ID: "default", Version: 5, Family: "admission.input", CompilerVersion: "v1",
		BodyHash: strings.Repeat("a", 64), SelectorsJSON: "{}", RequiredCapabilities: []string{"judge"},
	}
	baseDigest, err := PolicyResourceMessageDigest(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	again, err := PolicyResourceMessageDigest(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if baseDigest != again {
		t.Fatal("digest must be deterministic for identical fields")
	}

	variants := []func(*SignedPolicyResource){
		func(r *SignedPolicyResource) { r.Version = 6 },
		func(r *SignedPolicyResource) { r.Family = "egress.routing_guardrail" },
		func(r *SignedPolicyResource) { r.CompilerVersion = "v2" },
		func(r *SignedPolicyResource) { r.BodyHash = strings.Repeat("b", 64) },
		func(r *SignedPolicyResource) { r.SelectorsJSON = `{"team":"x"}` },
		func(r *SignedPolicyResource) { r.RequiredCapabilities = []string{"judge", "egress.route"} },
	}
	for i, mutate := range variants {
		r := base
		mutate(&r)
		d, err := PolicyResourceMessageDigest(r)
		if err != nil {
			t.Fatalf("variant %d: digest: %v", i, err)
		}
		if d == baseDigest {
			t.Errorf("variant %d: digest did not change — a field is not bound into the signing message", i)
		}
	}
}

// TestPolicyResourceSigningMessage_LengthPrefixFramingUnambiguous pins the
// length-prefixed framing choice: two different (field) splits that would
// concatenate to the same flat bytes under a naive delimiter must still
// produce different messages.
func TestPolicyResourceSigningMessage_LengthPrefixFramingUnambiguous(t *testing.T) {
	a, err := PolicyResourceSigningMessage("id", 1, "fam", "v1", "hash", "{\"a\":1}", nil)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := PolicyResourceSigningMessage("id", 1, "fam", "v1", "hash", "{\"a\":1}extra", nil)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if string(a) == string(b) {
		t.Fatal("differing selectors content must change the message")
	}
}
