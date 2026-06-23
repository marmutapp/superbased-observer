package orgcontract

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// Per-push proof headers. In addition to the possession-based bearer, every
// push carries an Ed25519 signature made with the agent's private key (whose
// public half was bound to the user record at enrolment). The server verifies
// the signature against that bound key, which defends against a stolen bearer
// being replayed by a party that does not also hold the agent's private key.
const (
	// HeaderAgentSignature carries base64url(Ed25519 signature) over the
	// canonical message from [PushSigningMessage].
	HeaderAgentSignature = "X-SBO-Agent-Signature"
	// HeaderTimestamp carries the Unix-seconds time the push was signed. The
	// server rejects pushes whose timestamp is outside ±[PushSignatureSkewSeconds]
	// to bound replay.
	HeaderTimestamp = "X-SBO-Timestamp"
	// PushSignatureSkewSeconds is the allowed clock skew for HeaderTimestamp.
	PushSignatureSkewSeconds int64 = 300
)

// PushSigningMessage returns the canonical bytes the agent signs and the
// server verifies: the signing timestamp (Unix seconds) and the SHA-256 of
// the exact body bytes on the wire (the gzip-compressed payload). Binding the
// signature to both the body hash and the timestamp makes a captured push
// non-replayable and tamper-evident. Client and server MUST derive the message
// identically — this single helper is the shared source of truth.
func PushSigningMessage(unixTimestamp int64, wireBody []byte) []byte {
	sum := sha256.Sum256(wireBody)
	return []byte(strconv.FormatInt(unixTimestamp, 10) + "\n" + hex.EncodeToString(sum[:]))
}

// Policy-bundle signing (guard spec §14.2). The org server signs every
// published bundle with a dedicated Ed25519 policy key (distinct from the
// bearer signing key — different rotation and exposure profiles); the agent
// verifies against the public key pinned at enrolment before any rule from
// the bundle can join its policy. Client and server MUST derive the canonical
// message identically — these helpers are the shared source of truth, exactly
// like PushSigningMessage above.

// policyBundleSigningPrefix domain-separates bundle signatures from every
// other Ed25519 use in the protocol (bearer envelopes, push proofs): a
// signature minted for one purpose can never verify for another.
const policyBundleSigningPrefix = "sbo-policy-bundle-v1"

// PolicyBundleSigningMessage returns the canonical bytes signed over a policy
// bundle: a fixed domain prefix, the bundle version, and the SHA-256 of the
// exact TOML bytes. Binding the version into the message means a captured
// signature for version N can never be replayed as a different version — a
// downgrade must present version N itself, which the agent's monotonic
// version check rejects.
func PolicyBundleSigningMessage(version int64, bundleTOML []byte) []byte {
	sum := sha256.Sum256(bundleTOML)
	return []byte(policyBundleSigningPrefix + "\n" +
		strconv.FormatInt(version, 10) + "\n" + hex.EncodeToString(sum[:]))
}

// SignPolicyBundle signs the canonical bundle message and returns the
// base64url signature in the wire encoding PolicyBundle.Signature carries.
// The server's publish path is the only caller.
func SignPolicyBundle(priv ed25519.PrivateKey, version int64, bundleTOML []byte) string {
	sig := ed25519.Sign(priv, PolicyBundleSigningMessage(version, bundleTOML))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyPolicyBundle checks a bundle envelope's signature against its own
// embedded public key: it decodes PublicKey and Signature from their wire
// encodings and verifies over the canonical message. It deliberately does NOT
// decide whether the embedded key is TRUSTED — callers compare
// sha256hex(decoded key) against their pinned hash (the agent's
// guard_policy_state pin row) before or after this check; the two checks
// together are the §14.2 acceptance gate. Returns the decoded public key so
// callers can hash-pin it without re-decoding.
func VerifyPolicyBundle(b PolicyBundle) (ed25519.PublicKey, error) {
	pub, err := base64.RawURLEncoding.DecodeString(b.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("orgcontract.VerifyPolicyBundle: decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("orgcontract.VerifyPolicyBundle: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	sig, err := base64.RawURLEncoding.DecodeString(b.Signature)
	if err != nil {
		return nil, fmt.Errorf("orgcontract.VerifyPolicyBundle: decode signature: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), PolicyBundleSigningMessage(b.Version, []byte(b.BundleTOML)), sig) {
		return nil, errors.New("orgcontract.VerifyPolicyBundle: signature verification failed")
	}
	return ed25519.PublicKey(pub), nil
}

// PublicKeyPinHash returns the sha256 hex of raw Ed25519 public-key bytes —
// the value pinned in the agent's guard_policy_state key-pin row and compared
// on every fetched bundle. One helper so enrolment pinning, fetch-time
// checking, and tests can never disagree on the hash recipe.
func PublicKeyPinHash(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}
