package orgcontract

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// Signed-distribution VERIFICATION for the two document rails the node
// consumes: the routing-policy rail and the org-announcement rail.
//
// These live here, in the dependency-free contract leaf, for the same
// reason the signing MESSAGE builders already do: the node
// (internal/orgclient) and the server (internal/orgserver/routingpolicy,
// internal/orgserver/organnounce) must derive the verified bytes
// identically, and the node must be able to verify WITHOUT importing any
// part of the server. Before this move, internal/orgclient imported
// internal/orgserver/{routingpolicy,organnounce} purely to reach these
// two functions, which made the node-side Teams client structurally
// dependent on the org-server tree.
//
// The server-side packages keep their exported Verify/VerifySigned
// entrypoints as thin delegations, so no server call site changed.

// VerifySignedBody is the document-shape-independent core of the
// routing-policy rail's verification: hash match first, then Ed25519
// over the BODY BYTES against the PINNED key.
//
// SECURITY NOTE (docs/security.md open ledger, ROUTING-SIG-1): the
// signature covers the body and NOTHING ELSE — not the version, not
// which rail the document arrived on. A captured signed policy can
// therefore be replayed at a different version number, and the org
// server's ONE signing identity means a signature minted on another
// body-signing rail would verify here too. This is a RELEASED wire
// format (agents in the field verify exactly these bytes), so it is
// recorded rather than changed in place; a fix is a versioned
// migration, not an edit.
//
// A NEW rail must NOT reuse this. The org-announcement rail
// ([VerifyAnnouncement], unreleased when it was hardened) verifies
// [AnnouncementSigningMessage] — domain tag + version + body —
// precisely so neither replay works there, and it deliberately does
// not call this function.
func VerifySignedBody(body, bodyHash, signatureB64, pinnedPubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(pinnedPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("orgcontract.VerifySignedBody: bad public key")
	}
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("orgcontract.VerifySignedBody: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(body))
	if hex.EncodeToString(sum[:]) != bodyHash {
		return fmt.Errorf("orgcontract.VerifySignedBody: body hash mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(body), sig) {
		return fmt.Errorf("orgcontract.VerifySignedBody: signature invalid")
	}
	return nil
}

// VerifyRoutingPolicy checks a routing-policy doc's V1 signature against
// a (pinned) public key. It is [VerifySignedBody] applied to the document
// shape.
//
// Callers that have a CHOICE must prefer [VerifyRoutingPolicyV2] whenever
// doc.SignatureV2 is non-empty, and must NOT fall back to this function
// when a v2 signature is present but fails — falling back would restore
// exactly the version-replay this rail's v2 exists to refuse. This one
// remains for documents served by a pre-v2 org server, which carry no v2
// signature at all.
func VerifyRoutingPolicy(doc RoutingPolicyDoc, pinnedPubB64 string) error {
	return VerifySignedBody(doc.Body, doc.BodyHash, doc.Signature, pinnedPubB64)
}

// VerifyRoutingPolicyV2 checks a routing-policy doc's V2 signature — the
// DOMAIN-SEPARATED, VERSION-BOUND one ([RoutingPolicySigningMessageV2]) —
// against a (pinned) public key. It closes docs/security.md ledger row
// ROUTING-SIG-1 for every document a v2-capable server serves:
//
//   - A genuinely signed policy can no longer be re-presented at a
//     DIFFERENT version. The freeze attack (bump a captured document's
//     version to a huge value so every later genuine publish loses the
//     agent's monotonic `cached.Version >= doc.Version` short-circuit)
//     now fails signature verification, because the version is inside
//     the signed message.
//   - A document signed on ANOTHER rail cannot be presented here, even
//     though ONE org key signs them all: v1 routing signs bare body
//     bytes and the announcement rail signs its own domain tag, so no
//     signature crosses.
//
// It REFUSES a document with an empty SignatureV2 rather than silently
// succeeding or silently downgrading: choosing between rails is the
// caller's decision (made on presence), and this function only ever
// answers for v2.
//
// BodyHash is still checked, but only as what it is — an integrity/dedup
// value. It authorizes nothing on its own, so it is verified BEFORE the
// signature purely to give the clearer error.
func VerifyRoutingPolicyV2(doc RoutingPolicyDoc, pinnedPubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(pinnedPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("orgcontract.VerifyRoutingPolicyV2: bad public key")
	}
	if doc.SignatureV2 == "" {
		return errors.New("orgcontract.VerifyRoutingPolicyV2: document carries no v2 signature")
	}
	sig, err := base64.StdEncoding.DecodeString(doc.SignatureV2)
	if err != nil {
		return errors.New("orgcontract.VerifyRoutingPolicyV2: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(doc.Body))
	if hex.EncodeToString(sum[:]) != doc.BodyHash {
		return errors.New("orgcontract.VerifyRoutingPolicyV2: body hash mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), RoutingPolicySigningMessageV2(doc.Version, doc.Body), sig) {
		return errors.New("orgcontract.VerifyRoutingPolicyV2: signature invalid (it must cover this rail's domain tag AND this version)")
	}
	return nil
}

// VerifyAnnouncement checks an announcement doc against a (pinned)
// public key.
//
// What the signature covers is the security-relevant part, and it is
// NOT the body alone: verification runs over
// [AnnouncementSigningMessage](doc.Version, doc.Body), which binds both
// the announcement RAIL (a domain tag) and the VERSION. Consequences,
// each one a real attack this refuses:
//
//   - A captured signed document cannot be replayed at a different
//     version. Bumping Version on a valid capture used to let an
//     eavesdropper freeze a fleet's cache at a huge version (no later
//     genuine announcement would ever pass the node's monotonic
//     short-circuit), or clear every banner by replaying an old
//     retraction. Both now fail signature verification.
//   - A routing-policy document cannot be presented as an announcement
//     (or vice versa) even though ONE org key signs both rails: the
//     routing rail signs the bare body, this rail signs the tagged
//     message, so neither signature verifies on the other's input.
//
// BodyHash is still checked, but only as what it is — an integrity/
// dedup value for display. It authorizes nothing on its own, so it is
// verified BEFORE the signature only to give the clearer error.
//
// The routing-policy rail is deliberately NOT changed to match: it is
// released, its signature shape is a compat surface, and the residual
// is recorded in docs/security.md's open ledger instead.
func VerifyAnnouncement(doc OrgAnnouncementDoc, pinnedPubB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(pinnedPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("orgcontract.VerifyAnnouncement: bad public key")
	}
	sig, err := base64.StdEncoding.DecodeString(doc.Signature)
	if err != nil {
		return errors.New("orgcontract.VerifyAnnouncement: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(doc.Body))
	if hex.EncodeToString(sum[:]) != doc.BodyHash {
		return errors.New("orgcontract.VerifyAnnouncement: body hash mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), AnnouncementSigningMessage(doc.Version, doc.Body), sig) {
		return errors.New("orgcontract.VerifyAnnouncement: signature invalid (it must cover this rail's domain tag AND this version)")
	}
	return nil
}
