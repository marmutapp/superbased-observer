package orgcontract

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// routingFixture mints a genuinely signed routing-policy document at the
// given version, carrying BOTH signature rails, exactly as the org server's
// Publish does. Every test below starts from a document the org really
// signed — the guard, not the setup, is what must refuse the attack.
func routingFixture(t *testing.T, version int64, body string) (RoutingPolicyDoc, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	doc := RoutingPolicyDoc{
		Version:     version,
		Body:        body,
		BodyHash:    hex.EncodeToString(sum[:]),
		Signature:   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))),
		SignatureV2: SignRoutingPolicyV2(priv, version, body),
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
	}
	return doc, doc.PublicKey
}

// TestRoutingPolicySigningMessageV2_BindsDomainAndVersion pins the v2
// primitive, mirroring TestAnnouncementSigningMessage_BindsDomainAndVersion:
// the message must move with the VERSION (so a captured signature cannot be
// replayed at another version) and must never equal a bare-body signing
// input or the announcement rail's message (so no signature crosses rails,
// even though ONE org key signs them all).
func TestRoutingPolicySigningMessageV2_BindsDomainAndVersion(t *testing.T) {
	const body = "[routing]\n"
	v1 := RoutingPolicySigningMessageV2(1, body)
	v2 := RoutingPolicySigningMessageV2(2, body)
	if bytes.Equal(v1, v2) {
		t.Error("the signing message ignores the version — a version-bumped replay would verify")
	}
	if bytes.Equal(v1, []byte(body)) {
		t.Error("the signing message is the bare body — the v1 replay survives into v2")
	}
	sum := sha256.Sum256([]byte(body))
	if bytes.Equal(v1, sum[:]) {
		t.Error("the signing message is a plain body hash — no domain separation")
	}
	if bytes.Equal(v1, AnnouncementSigningMessage(1, body)) {
		t.Error("routing v2 and the announcement rail derive the SAME message — one org key would make each rail's signature valid on the other")
	}
	if !bytes.Equal(v1, RoutingPolicySigningMessageV2(1, body)) {
		t.Error("the signing message is not deterministic")
	}
	// The NUL separators must make the encoding unambiguous: no body can
	// impersonate a different (version, body) split.
	if bytes.Equal(RoutingPolicySigningMessageV2(1, "2"+"\x00"+body), RoutingPolicySigningMessageV2(12, body)) {
		t.Error("version/body boundary is ambiguous")
	}
}

// TestVerifyRoutingPolicyV2_RefusesVersionReplay IS the regression test for
// docs/security.md ROUTING-SIG-1. The attack it reproduces: take a document
// the org GENUINELY signed at version N, change nothing but the version
// number, and serve it. Under v1 that verified — and an inflated version
// froze the node's cache against every later genuine publish, because the
// agent's monotonic `cached.Version >= doc.Version` short-circuit then
// discarded them all.
//
// Under v2 it must fail, and the v1 signature on the SAME document must
// still (demonstrably) accept it — which is what makes this a proof about
// the new rail rather than about a broken fixture.
func TestVerifyRoutingPolicyV2_RefusesVersionReplay(t *testing.T) {
	doc, pin := routingFixture(t, 7, "[routing]\n# genuine\n")

	if err := VerifyRoutingPolicyV2(doc, pin); err != nil {
		t.Fatalf("the genuine document must verify at its own version: %v", err)
	}

	// The replay: same body, same signatures, INFLATED version.
	replay := doc
	replay.Version = 1 << 40

	if err := VerifyRoutingPolicyV2(replay, pin); err == nil {
		t.Error("VERSION REPLAY ACCEPTED on the v2 rail — a genuinely signed old body re-presented at an inflated version would freeze the node's cache (ROUTING-SIG-1)")
	}
	// The N+1 case the ledger names, not only the absurd one.
	next := doc
	next.Version = doc.Version + 1
	if err := VerifyRoutingPolicyV2(next, pin); err == nil {
		t.Error("a v2 signature minted for version N verified at version N+1 — the version is not bound")
	}
	// A LOWER version (replaying an old policy over a newer one) is the
	// same defect in the other direction.
	prev := doc
	prev.Version = doc.Version - 1
	if err := VerifyRoutingPolicyV2(prev, pin); err == nil {
		t.Error("a v2 signature minted for version N verified at version N-1")
	}

	// The contrast that makes the finding concrete: the RELEASED v1 rail
	// accepts every one of those replays, which is precisely why v2 exists.
	if err := VerifyRoutingPolicy(replay, pin); err != nil {
		t.Errorf("v1 was expected to accept the replay (that IS the finding); it refused with %v — the fixture no longer reproduces ROUTING-SIG-1", err)
	}
}

// TestVerifyRoutingPolicyV2_CompatMatrix pins both compat directions plus
// the documented downgrade window (ledger ROUTING-SIG-2).
func TestVerifyRoutingPolicyV2_CompatMatrix(t *testing.T) {
	doc, pin := routingFixture(t, 3, "[routing]\n[[routing.privacy.rules]]\nproject = \"eu\"\n")

	t.Run("v2-bearing doc verifies on the v2 rail", func(t *testing.T) {
		if err := VerifyRoutingPolicyV2(doc, pin); err != nil {
			t.Errorf("genuine v2 doc refused: %v", err)
		}
	})

	t.Run("pre-v2 server doc (no v2 field) verifies on the legacy rail", func(t *testing.T) {
		old := doc
		old.SignatureV2 = ""
		if err := VerifyRoutingPolicy(old, pin); err != nil {
			t.Errorf("a pre-078 server's document must still verify on v1: %v", err)
		}
		// And v2 must REFUSE it rather than answering for a rail the
		// document does not ride: the presence check belongs to the
		// caller, and a silent success here would make "verified v2"
		// meaningless.
		if err := VerifyRoutingPolicyV2(old, pin); err == nil {
			t.Error("VerifyRoutingPolicyV2 accepted a document with no v2 signature")
		} else if !strings.Contains(err.Error(), "no v2 signature") {
			t.Errorf("unclear refusal for a missing v2 signature: %v", err)
		}
	})

	t.Run("tampered v2 signature fails", func(t *testing.T) {
		bad := doc
		// Flip one byte of the decoded signature and re-encode: a
		// well-formed base64 signature that is simply not this one.
		raw, derr := base64.StdEncoding.DecodeString(doc.SignatureV2)
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		raw[0] ^= 0xFF
		bad.SignatureV2 = base64.StdEncoding.EncodeToString(raw)
		if err := VerifyRoutingPolicyV2(bad, pin); err == nil {
			t.Error("tampered v2 signature verified")
		}
	})

	t.Run("tampered body fails on both rails", func(t *testing.T) {
		bad := doc
		bad.Body += "\n# evil\n"
		if err := VerifyRoutingPolicyV2(bad, pin); err == nil {
			t.Error("tampered body verified on v2")
		}
		if err := VerifyRoutingPolicy(bad, pin); err == nil {
			t.Error("tampered body verified on v1")
		}
		// Re-hashing the tampered body must not rescue it: BodyHash
		// authorizes nothing, the signature does.
		sum := sha256.Sum256([]byte(bad.Body))
		bad.BodyHash = hex.EncodeToString(sum[:])
		if err := VerifyRoutingPolicyV2(bad, pin); err == nil {
			t.Error("tampered body with a matching hash verified on v2 — BodyHash is being trusted as authorization")
		}
	})

	t.Run("wrong pinned key fails", func(t *testing.T) {
		otherPub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		if err := VerifyRoutingPolicyV2(doc, base64.StdEncoding.EncodeToString(otherPub)); err == nil {
			t.Error("v2 verified against a key that did not sign it")
		}
	})

	t.Run("malformed inputs fail", func(t *testing.T) {
		if err := VerifyRoutingPolicyV2(doc, "%%not-base64%%"); err == nil {
			t.Error("undecodable pinned key accepted")
		}
		if err := VerifyRoutingPolicyV2(doc, base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
			t.Error("wrong-size pinned key accepted")
		}
		badSig := doc
		badSig.SignatureV2 = "%%not-base64%%"
		if err := VerifyRoutingPolicyV2(badSig, pin); err == nil {
			t.Error("undecodable v2 signature accepted")
		}
	})

	// THE DOCUMENTED DOWNGRADE WINDOW (ledger ROUTING-SIG-2, accepted).
	// Until v2 becomes REQUIRED after a deprecation window, an attacker who
	// can serve the endpoint may simply STRIP signature_v2 and present the
	// v1 document — which still verifies, because it must, for pre-078
	// servers. This test pins that as KNOWN rather than leaving it to be
	// rediscovered as a surprise: if a future change makes v2 mandatory,
	// this test fails and the ledger row is what should be updated.
	t.Run("stripped v2 still legacy-verifies (accepted downgrade window)", func(t *testing.T) {
		stripped := doc
		stripped.SignatureV2 = ""
		if err := VerifyRoutingPolicy(stripped, pin); err != nil {
			t.Errorf("ROUTING-SIG-2's recorded residual no longer holds — v1 refused a stripped document (%v). If v2 was made mandatory, update the ledger row instead of this test.", err)
		}
	})
}

// TestSignRoutingPolicyV2_RoundTrips pins that the signer and the verifier
// derive the same message — the single-source-of-truth property every
// signing pair in this package depends on.
func TestSignRoutingPolicyV2_RoundTrips(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	const body = "[routing]\nmode = \"advise\"\n"
	for _, v := range []int64{0, 1, 42, 1 << 40} {
		sum := sha256.Sum256([]byte(body))
		doc := RoutingPolicyDoc{
			Version:     v,
			Body:        body,
			BodyHash:    hex.EncodeToString(sum[:]),
			SignatureV2: SignRoutingPolicyV2(priv, v, body),
		}
		if err := VerifyRoutingPolicyV2(doc, base64.StdEncoding.EncodeToString(pub)); err != nil {
			t.Errorf("version %d: round trip failed: %v", v, err)
		}
	}
}
