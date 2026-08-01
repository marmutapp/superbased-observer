package orgcontract

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

// TestAnnouncementSigningMessage_BindsDomainAndVersion pins security
// finding 1's primitive. The message must change when the VERSION
// changes (so a captured signature cannot be replayed at another
// version) and must never equal a bare-body signing input (so a
// signature minted on the routing rail, which signs body bytes with the
// SAME org key, cannot verify here).
func TestAnnouncementSigningMessage_BindsDomainAndVersion(t *testing.T) {
	const body = `{"id":"x"}`
	v1 := AnnouncementSigningMessage(1, body)
	v2 := AnnouncementSigningMessage(2, body)
	if bytes.Equal(v1, v2) {
		t.Error("the signing message ignores the version — a version-bumped replay would verify")
	}
	if bytes.Equal(v1, []byte(body)) {
		t.Error("the signing message is the bare body — cross-rail replay would verify")
	}
	sum := sha256.Sum256([]byte(body))
	if bytes.Equal(v1, sum[:]) {
		t.Error("the signing message is a plain body hash — no domain separation")
	}
	if !bytes.Equal(v1, AnnouncementSigningMessage(1, body)) {
		t.Error("the signing message is not deterministic")
	}
	// The NUL separators must make the encoding unambiguous: no body can
	// impersonate a different (version, body) split.
	if bytes.Equal(AnnouncementSigningMessage(1, "2"+"\x00"+body), AnnouncementSigningMessage(12, body)) {
		t.Error("version/body boundary is ambiguous")
	}
}

// TestDecodeCapped is security finding 5: a JSON decode over
// io.LimitReader caps the READ, not the document, and stops at the end
// of the first value — so trailing content and cap exhaustion both slip
// through as success.
func TestDecodeCapped(t *testing.T) {
	type doc struct {
		A string `json:"a"`
	}
	const cap20 = 20
	tests := []struct {
		name    string
		in      string
		max     int64
		wantErr string
		wantA   string
	}{
		{name: "plain document", in: `{"a":"x"}`, max: cap20, wantA: "x"},
		{name: "trailing newline (json.Encoder writes one)", in: `{"a":"x"}` + "\n", max: cap20, wantA: "x"},
		{name: "trailing whitespace", in: `{"a":"x"}` + "  \n\t", max: cap20, wantA: "x"},
		{name: "second document", in: `{"a":"x"}{"a":"y"}`, max: cap20, wantErr: "trailing bytes"},
		{name: "one trailing byte", in: `{"a":"x"}z`, max: cap20, wantErr: "trailing bytes"},
		{name: "trailing array", in: `{"a":"x"}[]`, max: cap20, wantErr: "trailing bytes"},
		// Boundary: a document of EXACTLY the cap decodes; one byte more
		// is refused as an over-cap document, not silently truncated.
		{name: "exactly at the cap", in: `{"a":"12345678901"}`, max: 19, wantA: "12345678901"},
		{name: "one byte over the cap", in: `{"a":"123456789012"}`, max: 19, wantErr: "exceeds"},
		{name: "far over the cap", in: `{"a":"` + strings.Repeat("p", 4096) + `"}`, max: 64, wantErr: "exceeds"},
		{name: "not json", in: `nope`, max: cap20, wantErr: "invalid character"},
		{name: "truncated json", in: `{"a":`, max: cap20, wantErr: "unexpected EOF"},
		{name: "non-positive cap", in: `{"a":"x"}`, max: 0, wantErr: "cap must be positive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got doc
			err := DecodeCapped(strings.NewReader(tc.in), tc.max, &got)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("DecodeCapped(%q) = nil error, want %q", tc.in, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("DecodeCapped(%q) error = %v, want substring %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeCapped(%q) = %v", tc.in, err)
			}
			if got.A != tc.wantA {
				t.Errorf("decoded a = %q, want %q", got.A, tc.wantA)
			}
		})
	}
}
