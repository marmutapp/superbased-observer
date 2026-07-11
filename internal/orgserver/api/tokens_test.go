package api

import (
	"strings"
	"testing"
)

func TestHashVerifyToken(t *testing.T) {
	tok := "the-secret-enrolment-token"
	enc, err := hashToken(tok)
	if err != nil {
		t.Fatalf("hashToken: %v", err)
	}
	if !strings.HasPrefix(enc, "$argon2id$") {
		t.Fatalf("unexpected encoding: %s", enc)
	}
	if !verifyToken(tok, enc) {
		t.Error("verifyToken rejected the correct token")
	}
	if verifyToken("wrong", enc) {
		t.Error("verifyToken accepted a wrong token")
	}
}

func TestVerifyTokenRejectsGarbageEncoding(t *testing.T) {
	for _, enc := range []string{"", "notphc", "$argon2id$bad", "$argon2i$v=19$m=1,t=1,p=1$AAAA$BBBB"} {
		if verifyToken("x", enc) {
			t.Errorf("verifyToken accepted garbage encoding %q", enc)
		}
	}
}

func TestHashTokenUniqueSalt(t *testing.T) {
	a, _ := hashToken("same")
	b, _ := hashToken("same")
	if a == b {
		t.Error("two hashes of the same token are identical — salt not random")
	}
}

func TestNewCleartextToken(t *testing.T) {
	a, err := newCleartextToken(32)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newCleartextToken(32)
	if a == b {
		t.Error("cleartext tokens not unique")
	}
	if len(a) < 40 { // 32 bytes base64url ≈ 43 chars
		t.Errorf("token too short: %d", len(a))
	}
}
