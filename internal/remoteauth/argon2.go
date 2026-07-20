package remoteauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP-recommended argon2id cost parameters (operator Q1 default; see doc.go).
const (
	argonMemoryKiB = 19 * 1024 // 19 MiB
	argonTime      = 2         // iterations
	argonThreads   = 1         // parallelism
	argonKeyLen    = 32        // derived-key bytes
	argonSaltLen   = 16        // salt bytes
	argonVersion   = argon2.Version
)

// ErrBadHash is returned by VerifySecret when the encoded hash is malformed.
var ErrBadHash = errors.New("remoteauth: malformed argon2id hash")

// HashSecret derives an argon2id hash of secret with a fresh random salt and
// returns it in the PHC string format
// ($argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>). The secret is stored hashed
// at rest; VerifySecret checks a candidate against it in constant time.
func HashSecret(secret []byte) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("remoteauth.HashSecret: rng failure: %w", err)
	}
	key := argon2.IDKey(secret, salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifySecret reports whether candidate matches the argon2id hash encoded in
// `encoded`, recomputing with the encoded parameters + salt and comparing in
// constant time. A malformed hash returns false (never a panic).
func VerifySecret(encoded string, candidate []byte) bool {
	m, t, p, salt, want, err := decodeArgon2(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey(candidate, salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// decodeArgon2 parses a PHC argon2id string into its parameters, salt and hash.
func decodeArgon2(encoded string) (mem uint32, time uint32, threads uint8, salt, hash []byte, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, ErrBadHash
	}
	var v int
	if _, e := fmt.Sscanf(parts[2], "v=%d", &v); e != nil || v != argonVersion {
		return 0, 0, 0, nil, nil, ErrBadHash
	}
	var mm, tt, pp int
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return 0, 0, 0, nil, nil, ErrBadHash
	}
	for _, item := range kv {
		nv := strings.SplitN(item, "=", 2)
		if len(nv) != 2 {
			return 0, 0, 0, nil, nil, ErrBadHash
		}
		n, e := strconv.Atoi(nv[1])
		if e != nil || n < 0 {
			return 0, 0, 0, nil, nil, ErrBadHash
		}
		switch nv[0] {
		case "m":
			mm = n
		case "t":
			tt = n
		case "p":
			pp = n
		default:
			return 0, 0, 0, nil, nil, ErrBadHash
		}
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	hash, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil || len(salt) == 0 || len(hash) == 0 || pp > 255 {
		return 0, 0, 0, nil, nil, ErrBadHash
	}
	return uint32(mm), uint32(tt), uint8(pp), salt, hash, nil
}
