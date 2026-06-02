package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Bearer validation errors. Callers (middleware) map all of these to HTTP
// 401; they are distinguished so logs and tests can tell them apart.
var (
	// ErrBearerMalformed means the token is not the expected
	// "payload.signature" base64url shape or its payload is not valid JSON.
	ErrBearerMalformed = errors.New("auth: bearer malformed")
	// ErrBearerBadSignature means the Ed25519 signature did not verify
	// against the server's public key.
	ErrBearerBadSignature = errors.New("auth: bearer signature invalid")
	// ErrBearerExpired means the token's exp is in the past (or iat is in
	// the future beyond the allowed skew).
	ErrBearerExpired = errors.New("auth: bearer expired")
)

// clockSkew is the tolerance applied to iat (issued-at) checks so a small
// clock difference between the minting and validating clocks (here always
// the same process, but kept for symmetry and future multi-instance setups)
// does not reject a freshly minted token.
const clockSkew = 60 * time.Second

// Issuer mints and validates Ed25519 bearers. It holds the server's signing
// key and the issuer string (the server's external URL). One key type, one
// algorithm; there is deliberately no algorithm field on the wire, so the
// "alg=none" and algorithm-confusion vulnerability classes do not exist.
type Issuer struct {
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	issuer   string
	lifetime time.Duration
	now      func() time.Time // injectable for tests
}

// NewIssuer constructs an Issuer. issuer is stamped into every bearer's iss
// claim; lifetime is the default bearer TTL.
func NewIssuer(priv ed25519.PrivateKey, issuer string, lifetime time.Duration) (*Issuer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("auth.NewIssuer: private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	if lifetime <= 0 {
		return nil, errors.New("auth.NewIssuer: lifetime must be > 0")
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("auth.NewIssuer: private key does not yield an Ed25519 public key")
	}
	return &Issuer{priv: priv, pub: pub, issuer: issuer, lifetime: lifetime, now: time.Now}, nil
}

// Mint signs a bearer for the given subject (SCIM user id) and audience (org
// id). It returns the wire token, the claims it encoded (so the caller can
// persist jti/exp), and any error. exp is now+lifetime; a fresh random jti
// makes each bearer individually revocable.
func (s *Issuer) Mint(sub, aud string) (string, orgcontract.BearerClaims, error) {
	jti, err := randomToken(16)
	if err != nil {
		return "", orgcontract.BearerClaims{}, fmt.Errorf("auth.Mint: jti: %w", err)
	}
	now := s.now().UTC()
	claims := orgcontract.BearerClaims{
		Iss: s.issuer,
		Sub: sub,
		Aud: aud,
		Iat: now.Unix(),
		Exp: now.Add(s.lifetime).Unix(),
		Jti: jti,
	}
	token, err := s.sign(claims)
	if err != nil {
		return "", orgcontract.BearerClaims{}, err
	}
	return token, claims, nil
}

func (s *Issuer) sign(claims orgcontract.BearerClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("auth.sign: marshal claims: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(s.priv, []byte(payloadB64))
	return payloadB64 + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Parse verifies a bearer's signature and that it is temporally valid, and
// returns its claims. It does NOT check revocation or that the subject still
// exists/is active — those are DB-backed checks the caller performs (jti
// against revoked_bearers, sub against org_members). Keeping them out of
// Parse keeps this function pure and unit-testable without a database.
//
// Ed25519 verification (ed25519.Verify) is itself constant-time, so no
// separate crypto/subtle comparison is needed here; subtle is used where we
// compare opaque secrets (session HMAC, SCIM token).
func (s *Issuer) Parse(token string) (orgcontract.BearerClaims, error) {
	return VerifyWithKey(s.pub, token, s.now())
}

// VerifyWithKey verifies a bearer against an explicit public key at the given
// reference time. Exposed so tests (and a future multi-key rotation path)
// can verify against a specific key without an Issuer.
func VerifyWithKey(pub ed25519.PublicKey, token string, ref time.Time) (orgcontract.BearerClaims, error) {
	var zero orgcontract.BearerClaims
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return zero, ErrBearerMalformed
	}
	payloadB64, sigB64 := token[:dot], token[dot+1:]

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return zero, ErrBearerMalformed
	}
	if !ed25519.Verify(pub, []byte(payloadB64), sig) {
		return zero, ErrBearerBadSignature
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return zero, ErrBearerMalformed
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	var claims orgcontract.BearerClaims
	if err := dec.Decode(&claims); err != nil {
		return zero, ErrBearerMalformed
	}

	ref = ref.UTC()
	if claims.Exp != 0 && ref.After(time.Unix(claims.Exp, 0)) {
		return zero, ErrBearerExpired
	}
	if claims.Iat != 0 && ref.Add(clockSkew).Before(time.Unix(claims.Iat, 0)) {
		// Issued in the future beyond skew — treat as not-yet-valid.
		return zero, ErrBearerExpired
	}
	return claims, nil
}

// randomToken returns nBytes of crypto/rand as a base64url (no padding)
// string. Used for jti and (via the api package) enrolment tokens.
func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// LoadSigningKey reads an Ed25519 private key from a PEM file. Both PKCS#8
// ("PRIVATE KEY") and the raw 64-byte seed||pub encoding ("ED25519 PRIVATE
// KEY", as written by GenerateSigningKeyPEM) are accepted.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth.LoadSigningKey: read %s: %w", path, err)
	}
	return ParseSigningKeyPEM(body)
}

// ParseSigningKeyPEM decodes an Ed25519 private key from PEM bytes.
func ParseSigningKeyPEM(body []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("auth.ParseSigningKeyPEM: no PEM block found")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth.ParseSigningKeyPEM: parse PKCS#8: %w", err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("auth.ParseSigningKeyPEM: PKCS#8 key is %T, want ed25519.PrivateKey", key)
		}
		return priv, nil
	case "ED25519 PRIVATE KEY":
		if len(block.Bytes) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("auth.ParseSigningKeyPEM: raw key is %d bytes, want %d", len(block.Bytes), ed25519.PrivateKeySize)
		}
		return ed25519.PrivateKey(block.Bytes), nil
	default:
		return nil, fmt.Errorf("auth.ParseSigningKeyPEM: unsupported PEM type %q", block.Type)
	}
}

// GenerateSigningKeyPEM creates a new Ed25519 private key and returns it both
// as a usable key and as PKCS#8 PEM bytes for writing to disk. Used by tests
// and by the observer-org key bootstrap path.
func GenerateSigningKeyPEM() (ed25519.PrivateKey, []byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.GenerateSigningKeyPEM: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("auth.GenerateSigningKeyPEM: marshal: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return priv, pemBytes, nil
}

// EncodePublicKey renders an Ed25519 public key as base64url (no padding),
// the wire form used for the agent's enrolment public key.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub)
}

// DecodePublicKey parses a base64url-encoded Ed25519 public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("auth.DecodePublicKey: base64url: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("auth.DecodePublicKey: %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
