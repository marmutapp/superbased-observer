package remoteauth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// SecretBytes is the pairing-secret length: 128 bits (plan §4.3).
const SecretBytes = 16

// GenerateSecret returns a fresh 128-bit pairing secret as raw bytes and its
// base64url (no-padding) encoding. The encoded form is what rides the QR /
// pairing URL FRAGMENT (client-side, never sent to or logged by the server);
// only its argon2id hash is stored at rest (HashSecret).
func GenerateSecret() (raw []byte, encoded string, err error) {
	raw = make([]byte, SecretBytes)
	if _, err = rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("remoteauth.GenerateSecret: %w", err)
	}
	return raw, base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeSecret parses a base64url-encoded pairing secret back to raw bytes for
// verification against the stored hash. A malformed or wrong-length input is an
// error (never a partial secret).
func DecodeSecret(encoded string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("remoteauth.DecodeSecret: %w", err)
	}
	if len(b) != SecretBytes {
		return nil, fmt.Errorf("remoteauth.DecodeSecret: got %d bytes, want %d", len(b), SecretBytes)
	}
	return b, nil
}

// GenerateCSRFToken returns a fresh 256-bit CSRF token (base64url) for a
// cookie-authenticated session (plan §4.5). Issued at pairing, required on
// cookie-auth mutations.
func GenerateCSRFToken() (string, error) {
	return randToken(32)
}

// randToken returns n bytes of crypto/rand as base64url (no padding). Used for
// session ids, execute-capability tokens and CSRF tokens.
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("remoteauth.randToken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
