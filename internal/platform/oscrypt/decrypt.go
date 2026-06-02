package oscrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
)

// Validator is invoked on each candidate plaintext during DecryptCTR's
// try-loop. Return true to accept the candidate and stop trying. The
// typical implementation is `protowire.ValidatesAsProto(buf, 10) > 5`.
type Validator func(plaintext []byte) bool

// DecryptResult carries the successful candidate's plaintext plus
// metadata about which strategy worked, useful for diagnostic logs.
type DecryptResult struct {
	Plaintext []byte
	// Strategy is a short label for the (cipher, key derivation, skip)
	// combo that produced a validating plaintext. Examples:
	// "aes-128-ctr.direct.skip0", "aes-128-ctr.pbkdf2.skip2".
	Strategy string
	// SkipBytes is the alignment-padding byte count consumed before
	// the protobuf root.
	SkipBytes int
}

// DecryptCTR runs the documented Antigravity decryption procedure
// against raw with a multi-strategy try-loop:
//
//  1. iv = raw[0:16]; ct = raw[16:]
//  2. for each (key, label) in candidate list:
//     for each skip in {0, 1, 2, 4, 8}:
//     plaintext = AES-CTR-decrypt(key, iv, ct)[skip:]
//     if validator(plaintext): return success
//  3. return error
//
// Candidate keys (in order):
//   - secret[:16] — direct AES-128
//   - secret (full, only if 16 ≤ len < 32 — handles odd-length keys)
//   - secret[:32] — direct AES-256, only if len(secret) >= 32
//   - PBKDF2(secret, "saltysalt", 1, 16) — derived AES-128
//   - PBKDF2(secret, "saltysalt", 1, 32) — derived AES-256
//
// AES-CBC and AES-GCM variants are NOT included here — they have
// different on-disk layouts (CBC needs PKCS#7 padding, GCM needs an
// auth tag). Callers wanting those modes should call the dedicated
// helpers (DecryptCBC, DecryptGCM) and fold their results into the
// caller-side strategy loop.
func DecryptCTR(raw, secret []byte, validator Validator) (*DecryptResult, error) {
	if len(raw) < 32 {
		return nil, errors.New("oscrypt.DecryptCTR: raw too small (needs 16-byte IV + ciphertext)")
	}
	iv := raw[:16]
	ct := raw[16:]

	type candidate struct {
		key   []byte
		label string
	}
	var candidates []candidate
	if len(secret) >= 16 {
		candidates = append(candidates, candidate{secret[:16], "direct16"})
	}
	if len(secret) >= 32 {
		candidates = append(candidates, candidate{secret[:32], "direct32"})
	}
	candidates = append(
		candidates,
		candidate{DeriveKey(secret), "pbkdf2-16"},
		candidate{DeriveKey256(secret), "pbkdf2-32"},
	)

	skips := []int{0, 1, 2, 4, 8}

	for _, c := range candidates {
		block, err := aes.NewCipher(c.key)
		if err != nil {
			continue
		}
		pt := make([]byte, len(ct))
		cipher.NewCTR(block, iv).XORKeyStream(pt, ct)

		for _, skip := range skips {
			if skip >= len(pt) {
				continue
			}
			body := pt[skip:]
			if validator(body) {
				return &DecryptResult{
					Plaintext: body,
					Strategy:  fmt.Sprintf("aes-ctr.%s.skip%d", c.label, skip),
					SkipBytes: skip,
				}, nil
			}
		}
	}
	return nil, errors.New("oscrypt.DecryptCTR: no candidate plaintext validated (wrong key, unknown cipher, or corrupt file)")
}

// DecryptCBC applies AES-CBC with PKCS#7 padding using raw[0:16] as
// the IV. Returns the unpadded plaintext on validator success. The
// strategy lineup mirrors DecryptCTR.
func DecryptCBC(raw, secret []byte, validator Validator) (*DecryptResult, error) {
	if len(raw) < 32 {
		return nil, errors.New("oscrypt.DecryptCBC: raw too small")
	}
	iv := raw[:16]
	ct := raw[16:]
	if len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("oscrypt.DecryptCBC: ciphertext length not a multiple of block size")
	}

	type candidate struct {
		key   []byte
		label string
	}
	var candidates []candidate
	if len(secret) >= 16 {
		candidates = append(candidates, candidate{secret[:16], "direct16"})
	}
	if len(secret) >= 32 {
		candidates = append(candidates, candidate{secret[:32], "direct32"})
	}
	candidates = append(
		candidates,
		candidate{DeriveKey(secret), "pbkdf2-16"},
		candidate{DeriveKey256(secret), "pbkdf2-32"},
	)

	skips := []int{0, 1, 2, 4, 8}

	for _, c := range candidates {
		block, err := aes.NewCipher(c.key)
		if err != nil {
			continue
		}
		pt := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
		// PKCS#7 unpad if last byte ≤ block size.
		body := pt
		if n := len(body); n > 0 {
			pad := int(body[n-1])
			if pad > 0 && pad <= aes.BlockSize && n >= pad {
				body = body[:n-pad]
			}
		}
		for _, skip := range skips {
			if skip >= len(body) {
				continue
			}
			if validator(body[skip:]) {
				return &DecryptResult{
					Plaintext: body[skip:],
					Strategy:  fmt.Sprintf("aes-cbc.%s.skip%d", c.label, skip),
					SkipBytes: skip,
				}, nil
			}
		}
	}
	return nil, errors.New("oscrypt.DecryptCBC: no candidate plaintext validated")
}

// DecryptGCM tries AES-GCM with several common nonce/tag layouts:
//
//	a. nonce[0:12] || ciphertext || tag[-16:]
//	b. nonce[0:16] || ciphertext || tag[-16:]
//
// Both AES-128 and AES-256 keys are tried (direct + PBKDF2-derived).
// Callers using GCM should NOT depend on raw[0:16] being an IV — GCM
// uses a 12-byte nonce by convention and the auth tag lives at the
// tail of the message.
func DecryptGCM(raw, secret []byte, validator Validator) (*DecryptResult, error) {
	if len(raw) < 32 {
		return nil, errors.New("oscrypt.DecryptGCM: raw too small")
	}

	type candidate struct {
		key   []byte
		label string
	}
	var candidates []candidate
	if len(secret) >= 16 {
		candidates = append(candidates, candidate{secret[:16], "direct16"})
	}
	if len(secret) >= 32 {
		candidates = append(candidates, candidate{secret[:32], "direct32"})
	}
	candidates = append(
		candidates,
		candidate{DeriveKey(secret), "pbkdf2-16"},
		candidate{DeriveKey256(secret), "pbkdf2-32"},
	)

	type layout struct {
		nonceLen, tagLen int
		label            string
	}
	layouts := []layout{
		{12, 16, "n12t16"},
		{16, 16, "n16t16"},
	}

	for _, c := range candidates {
		block, err := aes.NewCipher(c.key)
		if err != nil {
			continue
		}
		for _, lay := range layouts {
			if len(raw) < lay.nonceLen+lay.tagLen+1 {
				continue
			}
			nonce := raw[:lay.nonceLen]
			rest := raw[lay.nonceLen:]
			gcm, err := cipher.NewGCMWithNonceSize(block, lay.nonceLen)
			if err != nil {
				continue
			}
			pt, err := gcm.Open(nil, nonce, rest, nil)
			if err != nil {
				continue
			}
			if validator(pt) {
				return &DecryptResult{
					Plaintext: pt,
					Strategy:  fmt.Sprintf("aes-gcm.%s.%s", c.label, lay.label),
				}, nil
			}
		}
	}
	return nil, errors.New("oscrypt.DecryptGCM: no candidate plaintext validated")
}

// DecryptAuto runs DecryptCTR, DecryptCBC, and DecryptGCM in order
// and returns the first success. This matches the third-party
// arashz/antigravity_decryptor library's resilience pattern — the
// on-disk cipher has shifted between Antigravity versions, and we
// can't predict which the user's `.pb` files use without trying.
func DecryptAuto(raw, secret []byte, validator Validator) (*DecryptResult, error) {
	if res, err := DecryptCTR(raw, secret, validator); err == nil {
		return res, nil
	}
	if res, err := DecryptCBC(raw, secret, validator); err == nil {
		return res, nil
	}
	if res, err := DecryptGCM(raw, secret, validator); err == nil {
		return res, nil
	}
	return nil, errors.New("oscrypt.DecryptAuto: no cipher mode produced a validating plaintext")
}
