package orgcontract

import (
	"crypto/sha256"
	"encoding/hex"
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
