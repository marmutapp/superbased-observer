package remoteauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// StandingSecretPrefix marks the encoded standing terminal-control secret. It
// is a literal ASCII prefix ending in '.', a character OUTSIDE the base64url
// alphabet (A-Za-z0-9-_). A single-use capability token is pure base64url
// (randToken), so it can NEVER carry this prefix — the acquire boundary can
// therefore branch the reusable standing-secret path from the one-time
// capability path with zero collision risk, not merely a low probability.
const StandingSecretPrefix = "standing."

// StandingSecretBytes is the standing terminal-control secret length: 256 bits.
// Larger than the 128-bit pairing secret because the standing secret is a
// REUSABLE bearer the operator may store in a device's localStorage (§B
// warning), so it carries more residual exposure and earns full-strength
// entropy. Only its argon2id hash is stored at rest (HashSecret).
const StandingSecretBytes = 32

// GenerateStandingSecret returns a fresh 256-bit standing terminal-control
// secret as raw bytes and its wire-encoded form (StandingSecretPrefix +
// base64url-no-pad). The encoded form is what the LOCAL operator conveys ONCE
// to a paired device (shown once, stored hashed at rest — HashSecret); the
// device presents it verbatim on the writer-acquire frame's cap field, where
// IsStandingSecret routes it to the standing verification leg.
func GenerateStandingSecret() (raw []byte, encoded string, err error) {
	raw = make([]byte, StandingSecretBytes)
	if _, err = rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("remoteauth.GenerateStandingSecret: rand failure: %w", err)
	}
	return raw, StandingSecretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// IsStandingSecret reports whether an acquire-frame credential is a standing
// terminal-control secret (carries StandingSecretPrefix) rather than a one-time
// capability token. Used ONLY to ROUTE the credential at the acquire boundary;
// it is not itself an authorization decision.
func IsStandingSecret(cred string) bool {
	return strings.HasPrefix(cred, StandingSecretPrefix)
}

// DecodeStandingSecret parses a wire-encoded standing secret back to raw bytes
// for VerifySecret against the stored argon2id hash. A missing prefix, a
// malformed base64url body, or a wrong length is an error (never a partial
// secret), so a garbled credential can never accidentally verify.
func DecodeStandingSecret(encoded string) ([]byte, error) {
	if !strings.HasPrefix(encoded, StandingSecretPrefix) {
		return nil, fmt.Errorf("remoteauth.DecodeStandingSecret: missing %q prefix", StandingSecretPrefix)
	}
	body := strings.TrimPrefix(encoded, StandingSecretPrefix)
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("remoteauth.DecodeStandingSecret: decode: %w", err)
	}
	if len(b) != StandingSecretBytes {
		return nil, fmt.Errorf("remoteauth.DecodeStandingSecret: got %d bytes, want %d", len(b), StandingSecretBytes)
	}
	return b, nil
}
