package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters from the OWASP Password Storage cheat sheet
// (m=64MiB, t=3, p=4, 32-byte output). Used for the one-time enrolment
// tokens stored at rest; the cost is paid once at enrol, not per request.
const (
	argonMemory  = 64 * 1024 // KiB
	argonTime    = 3
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// hashToken hashes a cleartext token with argon2id and returns the standard
// PHC-string encoding ($argon2id$v=19$m=...,t=...,p=...$salt$hash).
func hashToken(token string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("api.hashToken: salt: %w", err)
	}
	sum := argon2.IDKey([]byte(token), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// verifyToken reports whether the cleartext token matches the PHC-encoded
// argon2id hash, using a constant-time comparison.
func verifyToken(token, encoded string) bool {
	mem, time32, threads, salt, want, err := decodeArgon2(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(token), salt, time32, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func decodeArgon2(encoded string) (mem, time32 uint32, threads uint8, salt, hash []byte, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, errors.New("api.decodeArgon2: not an argon2id PHC string")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return 0, 0, 0, nil, nil, errors.New("api.decodeArgon2: unsupported version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time32, &threads); err != nil {
		return 0, 0, 0, nil, nil, errors.New("api.decodeArgon2: bad parameters")
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return 0, 0, 0, nil, nil, errors.New("api.decodeArgon2: bad salt")
	}
	if hash, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return 0, 0, 0, nil, nil, errors.New("api.decodeArgon2: bad hash")
	}
	return mem, time32, threads, salt, hash, nil
}

// newTokenMaterial generates one enrolment token's material: the compound
// cleartext ("<token_id>.<secret>") handed to the invitee exactly once, the
// non-secret token id used as the lookup key, and the argon2id hash stored at
// rest.
//
// It is deliberately callable WITHOUT a transaction or a DB handle: argon2id
// at the OWASP parameters costs ~60ms, and running it while holding SQLite's
// write lock would turn the invite mint's serialization (see
// store.mintInviteAtomically) into a throughput cliff. Callers generate the
// material first and insert it inside the transaction.
func newTokenMaterial() (cleartext, tokenID, hash string, err error) {
	secret, err := newCleartextToken(32)
	if err != nil {
		return "", "", "", err
	}
	// Only the secret is hashed; the token_id is the (non-secret) lookup key.
	hash, err = hashToken(secret)
	if err != nil {
		return "", "", "", err
	}
	tokenID, err = randID()
	if err != nil {
		return "", "", "", err
	}
	// The inviter hands the developer this single compound string; the server
	// resolves the user from token_id at enrol, so no user_id is needed there.
	return tokenID + "." + secret, tokenID, hash, nil
}

// newCleartextToken returns nBytes of crypto/rand as a base64url (no padding)
// string — the cleartext enrolment token handed to an admin once.
func newCleartextToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
