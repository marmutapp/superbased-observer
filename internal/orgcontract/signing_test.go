package orgcontract

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestPushSigningMessage_Deterministic(t *testing.T) {
	body := []byte("gzip-bytes-here")
	a := PushSigningMessage(1748000000, body)
	b := PushSigningMessage(1748000000, body)
	if !bytes.Equal(a, b) {
		t.Fatal("same inputs must produce the same message")
	}
}

func TestPushSigningMessage_BindsTimestampAndBody(t *testing.T) {
	body := []byte("payload")
	base := PushSigningMessage(1748000000, body)

	if bytes.Equal(base, PushSigningMessage(1748000001, body)) {
		t.Fatal("a different timestamp must change the message")
	}
	if bytes.Equal(base, PushSigningMessage(1748000000, []byte("payload2"))) {
		t.Fatal("a different body must change the message")
	}
	// Shape: "<ts>\n<64 hex chars>".
	nl := bytes.IndexByte(base, '\n')
	if nl < 0 {
		t.Fatalf("message missing newline separator: %q", base)
	}
	if got := len(base) - nl - 1; got != 64 {
		t.Fatalf("hash segment = %d chars, want 64 (sha256 hex)", got)
	}
}

func TestPolicyBundleSigningMessage_BindsVersionAndBody(t *testing.T) {
	toml := []byte("[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\n")
	base := PolicyBundleSigningMessage(3, toml)

	if !bytes.HasPrefix(base, []byte("sbo-policy-bundle-v1\n")) {
		t.Fatalf("message missing the domain-separation prefix: %q", base)
	}
	if bytes.Equal(base, PolicyBundleSigningMessage(4, toml)) {
		t.Fatal("a different version must change the message")
	}
	if bytes.Equal(base, PolicyBundleSigningMessage(3, []byte("other"))) {
		t.Fatal("a different bundle body must change the message")
	}
	if bytes.Equal(base, PolicyBundleSigningMessage(3, toml)) != true {
		t.Fatal("same inputs must produce the same message")
	}
	// Bundle and push messages over comparable inputs never collide —
	// the prefix domain-separates the two signature uses.
	if bytes.Equal(PolicyBundleSigningMessage(1748000000, toml), PushSigningMessage(1748000000, toml)) {
		t.Fatal("bundle and push canonical messages must be domain-separated")
	}
}

// TestPolicyBundleSignVerify pins the sign→verify round trip and every
// rejection class VerifyPolicyBundle owns: tampered body, tampered version,
// wrong key, malformed encodings. One case per row (§18 per-rule style).
func TestPolicyBundleSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	const toml = "[[override]]\nrule = \"R-110\"\ndecision = \"deny\"\n"
	good := PolicyBundle{
		Version:    7,
		BundleTOML: toml,
		Signature:  SignPolicyBundle(priv, 7, []byte(toml)),
		PublicKey:  base64.RawURLEncoding.EncodeToString(pub),
		SignedAt:   "2026-06-11T09:00:00Z",
	}

	cases := []struct {
		name    string
		mutate  func(b *PolicyBundle)
		wantErr string // "" = verify must succeed
	}{
		{"valid round trip", func(*PolicyBundle) {}, ""},
		{"tampered bundle body", func(b *PolicyBundle) { b.BundleTOML += "# evil\n" }, "signature verification failed"},
		{"tampered version", func(b *PolicyBundle) { b.Version = 8 }, "signature verification failed"},
		{"wrong key", func(b *PolicyBundle) {
			b.PublicKey = base64.RawURLEncoding.EncodeToString(otherPub)
		}, "signature verification failed"},
		{"malformed public key encoding", func(b *PolicyBundle) { b.PublicKey = "!!!" }, "decode public key"},
		{"truncated public key", func(b *PolicyBundle) { b.PublicKey = "cHVi" }, "public key is 3 bytes"},
		{"malformed signature encoding", func(b *PolicyBundle) { b.Signature = "!!!" }, "decode signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := good
			tc.mutate(&b)
			gotPub, err := VerifyPolicyBundle(b)
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

func TestPublicKeyPinHash(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	h := PublicKeyPinHash(pub)
	if len(h) != 64 {
		t.Fatalf("pin hash = %d chars, want 64 (sha256 hex)", len(h))
	}
	if h != PublicKeyPinHash(pub) {
		t.Fatal("pin hash must be deterministic")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if h == PublicKeyPinHash(other) {
		t.Fatal("distinct keys must pin differently")
	}
}
