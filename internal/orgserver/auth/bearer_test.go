package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestIssuer(t *testing.T) (*Issuer, ed25519.PublicKey) {
	t.Helper()
	priv, _, err := GenerateSigningKeyPEM()
	if err != nil {
		t.Fatalf("GenerateSigningKeyPEM: %v", err)
	}
	iss, err := NewIssuer(priv, "https://org.example", 90*24*time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss, priv.Public().(ed25519.PublicKey)
}

func TestMintParseRoundTrip(t *testing.T) {
	iss, _ := newTestIssuer(t)
	token, claims, err := iss.Mint("user-123", "org-abc")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.Contains(token, ".") {
		t.Fatalf("token missing separator: %q", token)
	}
	if claims.Sub != "user-123" || claims.Aud != "org-abc" || claims.Iss != "https://org.example" {
		t.Errorf("unexpected claims: %+v", claims)
	}
	if claims.Jti == "" {
		t.Error("jti empty")
	}
	if claims.Exp <= claims.Iat {
		t.Errorf("exp %d not after iat %d", claims.Exp, claims.Iat)
	}

	got, err := iss.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != claims {
		t.Errorf("parsed claims %+v != minted %+v", got, claims)
	}
}

func TestUniqueJTI(t *testing.T) {
	iss, _ := newTestIssuer(t)
	_, c1, _ := iss.Mint("u", "o")
	_, c2, _ := iss.Mint("u", "o")
	if c1.Jti == c2.Jti {
		t.Error("jti not unique across mints")
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	iss, _ := newTestIssuer(t)
	token, _, _ := iss.Mint("user-123", "org-abc")
	parts := strings.SplitN(token, ".", 2)

	// Re-encode a payload claiming a different subject, keep the old sig.
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://org.example","sub":"attacker","aud":"org-abc","exp":9999999999,"iat":1,"jti":"x"}`))
	tampered := forged + "." + parts[1]
	if _, err := iss.Parse(tampered); !errors.Is(err, ErrBearerBadSignature) {
		t.Errorf("tampered payload: err = %v, want ErrBearerBadSignature", err)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	iss, _ := newTestIssuer(t)
	token, _, _ := iss.Mint("u", "o")

	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyWithKey(otherPub, token, time.Now()); !errors.Is(err, ErrBearerBadSignature) {
		t.Errorf("wrong key: err = %v, want ErrBearerBadSignature", err)
	}
}

func TestExpiredRejected(t *testing.T) {
	priv, _, _ := GenerateSigningKeyPEM()
	iss, _ := NewIssuer(priv, "iss", time.Hour)
	// Mint at a fixed past time so exp is well behind ref-now.
	iss.now = func() time.Time { return time.Unix(1_000_000, 0) }
	token, _, _ := iss.Mint("u", "o")

	got, err := VerifyWithKey(priv.Public().(ed25519.PublicKey), token, time.Unix(2_000_000, 0))
	if !errors.Is(err, ErrBearerExpired) {
		t.Errorf("expired: err = %v, want ErrBearerExpired (claims %+v)", err, got)
	}
}

func TestFutureIssuedRejected(t *testing.T) {
	priv, _, _ := GenerateSigningKeyPEM()
	iss, _ := NewIssuer(priv, "iss", time.Hour)
	iss.now = func() time.Time { return time.Unix(5_000_000, 0) }
	token, _, _ := iss.Mint("u", "o")

	// Validate as if it were well before iat (beyond clock skew).
	if _, err := VerifyWithKey(priv.Public().(ed25519.PublicKey), token, time.Unix(1_000_000, 0)); !errors.Is(err, ErrBearerExpired) {
		t.Errorf("future-issued: err = %v, want ErrBearerExpired", err)
	}
}

func TestMalformedTokens(t *testing.T) {
	iss, _ := newTestIssuer(t)
	for _, tok := range []string{"", ".", "noseparator", "a.", ".b", "not-base64!.also-not", strings.Repeat("a", 10) + "." + strings.Repeat("b", 10)} {
		if _, err := iss.Parse(tok); !errors.Is(err, ErrBearerMalformed) && !errors.Is(err, ErrBearerBadSignature) {
			t.Errorf("token %q: err = %v, want malformed or bad-signature", tok, err)
		}
	}
}

func TestPEMRoundTrip(t *testing.T) {
	priv, pemBytes, err := GenerateSigningKeyPEM()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	parsed, err := ParseSigningKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !priv.Equal(parsed) {
		t.Error("PEM round-trip changed the key")
	}
}

func TestPublicKeyEncoding(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	enc := EncodePublicKey(pub)
	got, err := DecodePublicKey(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !pub.Equal(got) {
		t.Error("public key encode/decode mismatch")
	}
	if _, err := DecodePublicKey("tooshort"); err == nil {
		t.Error("expected error decoding short key")
	}
}

func TestNewIssuerRejectsBadInput(t *testing.T) {
	priv, _, _ := GenerateSigningKeyPEM()
	if _, err := NewIssuer(priv, "iss", 0); err == nil {
		t.Error("expected error for zero lifetime")
	}
	if _, err := NewIssuer(ed25519.PrivateKey{1, 2, 3}, "iss", time.Hour); err == nil {
		t.Error("expected error for short key")
	}
}
