package orgcontract

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// ACP-P6c §3e: the enrolment grant carries CONSENT EVIDENCE, and the signing
// message is PRESENCE-VERSIONED so adding it cost nothing on the token rail.
//
// The gate these tests defend is compatibility, not cryptography: if the
// consent writes ever became unconditional, every grant an older server signs
// (and every grant already stored on a node) would stop verifying, and the
// node's honest response to that is to enrol UNGOVERNED — a silent
// fleet-wide de-governance shipped as a "field addition".

// goldenConsentFreeSigningMessage is the sha256 of goldenGrant()'s signing
// message computed with the PRE-P6c algorithm — the field sequence with NO
// consent writes at all. It is hardcoded rather than derived so that a change
// to EnrolmentGrantSigningMessage cannot quietly move the target with the
// test that guards it.
const goldenConsentFreeSigningMessage = "b34236656191562d6e556eab1981c2fb7279a4a4e5fb9d4980a38559557c2c72"

// goldenGrant is a FIXED token-rail grant. Every field is a literal: the
// golden above is only meaningful if nothing about this value can drift.
func goldenGrant() EnrolmentGrant {
	return EnrolmentGrant{
		OrgID:        "org-golden",
		OrgServerURL: "https://org.example",
		KeyPinSHA256: "d0d0d0d0",
		Authority:    []string{"dashboard.visibility", "settings.pin"},
		GrantedAt:    "2026-08-20T00:00:00Z",
		ExpiresAt:    "2026-09-19T00:00:00Z",
	}
}

// legacySigningMessage is the pre-P6c algorithm, reproduced here in full. The
// golden constant pins the value; this reproduction pins the SHAPE, so a
// reviewer can see exactly which byte sequence the current implementation is
// being held to.
func legacySigningMessage(g EnrolmentGrant) []byte {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(enrolmentGrantSigningDomain)
	write(g.OrgID)
	write(g.OrgServerURL)
	write(g.KeyPinSHA256)
	for _, a := range CanonicalAuthority(g.Authority) {
		write("authority")
		write(a)
	}
	write(g.GrantedAt)
	write(g.ExpiresAt)
	return h.Sum(nil)
}

// TestSigningMessageByteIdenticalWithoutConsent is THE compat golden: a grant
// carrying no consent evidence must hash to exactly what the pre-P6c
// algorithm produced.
func TestSigningMessageByteIdenticalWithoutConsent(t *testing.T) {
	g := goldenGrant()
	got := hex.EncodeToString(EnrolmentGrantSigningMessage(g))
	if got != goldenConsentFreeSigningMessage {
		t.Fatalf("signing message for a consent-free grant = %s, want the pre-P6c golden %s\n"+
			"a grant with no consent evidence MUST hash exactly as it did before ACP-P6c, or every\n"+
			"already-signed grant stops verifying and every node enrols ungoverned",
			got, goldenConsentFreeSigningMessage)
	}
	if want := hex.EncodeToString(legacySigningMessage(g)); got != want {
		t.Fatalf("signing message = %s, pre-P6c algorithm = %s", got, want)
	}

	// An empty ConsentActor alongside an empty ConsentMode is still the
	// consent-free shape: presence is keyed on the MODE, which is the field
	// that decides whether evidence exists at all.
	g.ConsentActor = ""
	if got2 := hex.EncodeToString(EnrolmentGrantSigningMessage(g)); got2 != goldenConsentFreeSigningMessage {
		t.Fatalf("explicitly-empty consent actor changed the message: %s", got2)
	}
}

// TestSigningMessageBindsConsentEvidence: once present, the evidence is
// actually covered — each field independently changes the message.
func TestSigningMessageBindsConsentEvidence(t *testing.T) {
	base := goldenGrant()
	idp := base
	idp.ConsentMode = "idp"
	idp.ConsentActor = "dev@acme.example"

	baseMsg := hex.EncodeToString(EnrolmentGrantSigningMessage(base))
	idpMsg := hex.EncodeToString(EnrolmentGrantSigningMessage(idp))
	if idpMsg == baseMsg {
		t.Fatal("consent evidence is not covered by the signing message")
	}

	otherActor := idp
	otherActor.ConsentActor = "attacker@evil.example"
	if hex.EncodeToString(EnrolmentGrantSigningMessage(otherActor)) == idpMsg {
		t.Fatal("consent actor is not covered by the signing message")
	}
	otherMode := idp
	otherMode.ConsentMode = "managed"
	if hex.EncodeToString(EnrolmentGrantSigningMessage(otherMode)) == idpMsg {
		t.Fatal("consent mode is not covered by the signing message")
	}
}

// TestIdPGrantSignVerifyRoundTrip: an idp-minted grant verifies under the key
// that signed it, and its receipt hash differs from the same grant without
// evidence (the receipt is what an auditor recomputes).
func TestIdPGrantSignVerifyRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	g := goldenGrant()
	g.ConsentMode = "idp"
	g.ConsentActor = "dev@acme.example"
	g.Signature = SignEnrolmentGrant(priv, g)

	if err := VerifyEnrolmentGrant(g, pub); err != nil {
		t.Fatalf("VerifyEnrolmentGrant on an idp grant: %v", err)
	}

	bare := goldenGrant()
	bare.Signature = SignEnrolmentGrant(priv, bare)
	if EnrolmentGrantReceiptHash(g) == EnrolmentGrantReceiptHash(bare) {
		t.Fatal("receipt hash does not distinguish an idp grant from a token-rail grant")
	}
}

// TestTamperedConsentFailsVerification is the point of binding the evidence
// into the signature: an intermediary that rewrites who consented, or
// upgrades a token-rail grant into a managed-class one, must produce a grant
// the node refuses.
func TestTamperedConsentFailsVerification(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	signed := goldenGrant()
	signed.ConsentMode = "idp"
	signed.ConsentActor = "dev@acme.example"
	signed.Signature = SignEnrolmentGrant(priv, signed)

	cases := []struct {
		name string
		mut  func(g *EnrolmentGrant)
	}{
		{"actor rewritten", func(g *EnrolmentGrant) { g.ConsentActor = "attacker@evil.example" }},
		{"mode rewritten", func(g *EnrolmentGrant) { g.ConsentMode = "managed" }},
		{"evidence stripped", func(g *EnrolmentGrant) { g.ConsentMode, g.ConsentActor = "", "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := signed
			tc.mut(&g)
			if err := VerifyEnrolmentGrant(g, pub); err == nil {
				t.Fatal("tampered consent evidence verified — the signature does not bind it")
			}
		})
	}

	// The inverse: a token-rail grant cannot be UPGRADED into managed-class
	// consent by adding the fields to a document signed without them.
	bare := goldenGrant()
	bare.Signature = SignEnrolmentGrant(priv, bare)
	forged := bare
	forged.ConsentMode = "idp"
	forged.ConsentActor = "attacker@evil.example"
	if err := VerifyEnrolmentGrant(forged, pub); err == nil {
		t.Fatal("consent evidence could be added to an already-signed token-rail grant")
	}
}
