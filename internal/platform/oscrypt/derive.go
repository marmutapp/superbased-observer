package oscrypt

import (
	"crypto/sha1" //nolint:gosec // G505: Chromium's PBKDF2 derivation mandates SHA-1 for compatibility; the OS-protected secret — not the hash — provides the security.

	"golang.org/x/crypto/pbkdf2"
)

// DeriveKey applies Chromium's PBKDF2 derivation to secret. Returns
// a 16-byte AES-128 key. The salt + iteration count + output length
// are fixed Chromium constants — not chosen for security strength
// (the secret itself is OS-protected) but for compatibility.
func DeriveKey(secret []byte) []byte {
	return pbkdf2.Key(secret, []byte("saltysalt"), 1, 16, sha1.New)
}

// DeriveKey256 produces a 32-byte AES-256 key under the same
// Chromium PBKDF2 parameters. Some app variants store a longer
// secret-derived key for AES-256-CTR / AES-256-GCM modes.
func DeriveKey256(secret []byte) []byte {
	return pbkdf2.Key(secret, []byte("saltysalt"), 1, 32, sha1.New)
}
