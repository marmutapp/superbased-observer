package oscrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"testing"
)

// trivialValidator accepts any plaintext beginning with the magic
// prefix "TESTOK" — used to disambiguate "right key" from "random
// CTR garbage" without depending on protowire.
func trivialValidator(prefix []byte) Validator {
	return func(pt []byte) bool {
		if len(pt) < len(prefix) {
			return false
		}
		for i, b := range prefix {
			if pt[i] != b {
				return false
			}
		}
		return true
	}
}

func TestDeriveKeyMatchesChromiumDefaults(t *testing.T) {
	// PBKDF2-HMAC-SHA1("hello", "saltysalt", 1, 16). Cross-checked
	// against `openssl kdf -kdfopt digest:SHA1 -kdfopt pass:hello
	// -kdfopt salt:saltysalt -kdfopt iter:1 -keylen 16 PBKDF2` and
	// against Python `hashlib.pbkdf2_hmac('sha1', b'hello', b'saltysalt', 1, 16)`.
	got := DeriveKey([]byte("hello"))
	want, _ := hex.DecodeString("33021245925d3bc13e373805e78dd70b")
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("DeriveKey mismatch: got %x want %x", got, want)
	}
}

func TestDecryptCTRRoundTrip(t *testing.T) {
	// Synthesize: 16-byte IV + AES-128-CTR-encrypt("TESTOKsome plaintext")
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("rand: %v", err)
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	plaintext := []byte("TESTOKhello world this is the plaintext payload")
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(ct, plaintext)
	raw := slices.Concat(iv, ct)

	res, err := DecryptCTR(raw, key, trivialValidator([]byte("TESTOK")))
	if err != nil {
		t.Fatalf("DecryptCTR: %v", err)
	}
	if string(res.Plaintext) != string(plaintext) {
		t.Fatalf("plaintext mismatch: got %q want %q", res.Plaintext, plaintext)
	}
	if res.Strategy == "" {
		t.Fatal("Strategy empty")
	}
}

func TestDecryptCTRWrongKey(t *testing.T) {
	iv := make([]byte, 16)
	rand.Read(iv)
	rightKey := make([]byte, 16)
	rand.Read(rightKey)
	wrongKey := make([]byte, 16)
	rand.Read(wrongKey)
	plaintext := []byte("TESTOKpayload that needs the magic prefix")
	block, _ := aes.NewCipher(rightKey)
	ct := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(ct, plaintext)
	raw := slices.Concat(iv, ct)

	if _, err := DecryptCTR(raw, wrongKey, trivialValidator([]byte("TESTOK"))); err == nil {
		t.Fatal("DecryptCTR with wrong key returned nil error — must reject")
	}
}

func TestDecryptCTRSkipBytesAccepted(t *testing.T) {
	// Encrypt with a 4-byte alignment prefix before the magic.
	iv := make([]byte, 16)
	rand.Read(iv)
	key := make([]byte, 16)
	rand.Read(key)
	plaintext := append([]byte{0xAA, 0xBB, 0xCC, 0xDD}, []byte("TESTOKhello after alignment skip")...)
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(ct, plaintext)
	raw := slices.Concat(iv, ct)

	res, err := DecryptCTR(raw, key, trivialValidator([]byte("TESTOK")))
	if err != nil {
		t.Fatalf("DecryptCTR: %v", err)
	}
	if res.SkipBytes != 4 {
		t.Fatalf("expected skip=4, got skip=%d strategy=%s", res.SkipBytes, res.Strategy)
	}
}

func TestDecryptCBCRoundTrip(t *testing.T) {
	iv := make([]byte, 16)
	rand.Read(iv)
	key := make([]byte, 16)
	rand.Read(key)
	// PKCS#7-pad to block size.
	plaintext := []byte("TESTOKhello CBC plaintext payload")
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	for i := 0; i < pad; i++ {
		plaintext = append(plaintext, byte(pad))
	}
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(plaintext))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plaintext)
	raw := slices.Concat(iv, ct)

	res, err := DecryptCBC(raw, key, trivialValidator([]byte("TESTOK")))
	if err != nil {
		t.Fatalf("DecryptCBC: %v", err)
	}
	if got := string(res.Plaintext); got[:6] != "TESTOK" {
		t.Fatalf("plaintext prefix mismatch: %q", got)
	}
}

func TestDecryptGCMRoundTrip(t *testing.T) {
	key := make([]byte, 16)
	rand.Read(key)
	nonce := make([]byte, 12)
	rand.Read(nonce)
	plaintext := []byte("TESTOKgcm plaintext payload, well-protected")

	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCMWithNonceSize(block, 12)
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	raw := slices.Concat(nonce, sealed)

	res, err := DecryptGCM(raw, key, trivialValidator([]byte("TESTOK")))
	if err != nil {
		t.Fatalf("DecryptGCM: %v", err)
	}
	if string(res.Plaintext)[:6] != "TESTOK" {
		t.Fatalf("plaintext mismatch: %q", res.Plaintext)
	}
}

func TestDecryptAutoFallsThrough(t *testing.T) {
	// CTR-encrypted blob that decrypts cleanly under DecryptCTR.
	// DecryptAuto should pick CTR first and succeed.
	iv := make([]byte, 16)
	rand.Read(iv)
	key := make([]byte, 16)
	rand.Read(key)
	plaintext := []byte("TESTOKauto fallthrough check")
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(ct, plaintext)
	raw := slices.Concat(iv, ct)

	res, err := DecryptAuto(raw, key, trivialValidator([]byte("TESTOK")))
	if err != nil {
		t.Fatalf("DecryptAuto: %v", err)
	}
	if res.Strategy[:7] != "aes-ctr" {
		t.Fatalf("expected ctr strategy, got %q", res.Strategy)
	}
}

func TestSecretZero(t *testing.T) {
	s := Secret([]byte("topsecret"))
	s.Zero()
	for i, b := range s {
		if b != 0 {
			t.Fatalf("Zero left byte %d = %x", i, b)
		}
	}
}

func TestDoDecodeBase64HandlesWhitespace(t *testing.T) {
	got, err := doDecodeBase64("  aGVsbG8=  \n\r")
	if err != nil {
		t.Fatalf("doDecodeBase64: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q want hello", got)
	}
}
