package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultSessionCookie is the name of the SAML session cookie.
const DefaultSessionCookie = "sbo_org_session"

// DefaultSessionTTL is the default dashboard session lifetime (spec §2.3).
const DefaultSessionTTL = 12 * time.Hour

// minSessionKeyLen is the minimum acceptable HMAC key length. 32 bytes is the
// SHA-256 block-aligned minimum for a meaningful MAC key.
const minSessionKeyLen = 32

// ErrNoSession means the request carried no session cookie, or one that did
// not validate (bad signature, expired, malformed). Callers map this to "not
// logged in" — a redirect to SSO for dashboard routes.
var ErrNoSession = errors.New("auth: no valid session")

// sessionPayload is the cookie's signed body: who, and until when.
type sessionPayload struct {
	UserID string `json:"user_id"`
	Exp    int64  `json:"exp"` // Unix seconds
}

// SessionManager issues and validates HMAC-SHA256-signed session cookies. The
// cookie value is base64url(payload).base64url(hmac); there is no
// server-side session store — the cookie is self-contained, like the bearer.
// No third-party session library is used.
type SessionManager struct {
	key    []byte
	name   string
	ttl    time.Duration
	secure bool
	now    func() time.Time
}

// NewSessionManager constructs a SessionManager. secure should be true when
// the server is reached over HTTPS (it gates the cookie Secure attribute).
func NewSessionManager(key []byte, ttl time.Duration, secure bool) (*SessionManager, error) {
	if len(key) < minSessionKeyLen {
		return nil, fmt.Errorf("auth.NewSessionManager: session key is %d bytes, want >= %d", len(key), minSessionKeyLen)
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionManager{key: key, name: DefaultSessionCookie, ttl: ttl, secure: secure, now: time.Now}, nil
}

// Issue writes a fresh session cookie for userID onto w.
func (m *SessionManager) Issue(w http.ResponseWriter, userID string) error {
	payload := sessionPayload{UserID: userID, Exp: m.now().Add(m.ttl).UTC().Unix()}
	value, err := m.encode(payload)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.name,
		Value:    value,
		Path:     "/",
		Expires:  time.Unix(payload.Exp, 0),
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Clear expires the session cookie (logout).
func (m *SessionManager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// UserID validates the request's session cookie and returns the user id.
// Returns ErrNoSession for any failure (absent, malformed, bad MAC, expired).
func (m *SessionManager) UserID(r *http.Request) (string, error) {
	c, err := r.Cookie(m.name)
	if err != nil {
		return "", ErrNoSession
	}
	payload, err := m.decode(c.Value)
	if err != nil {
		return "", ErrNoSession
	}
	if m.now().UTC().Unix() >= payload.Exp {
		return "", ErrNoSession
	}
	return payload.UserID, nil
}

func (m *SessionManager) encode(p sessionPayload) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("auth.SessionManager.encode: %w", err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(body)
	mac := m.sign(b64)
	return b64 + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

func (m *SessionManager) decode(value string) (sessionPayload, error) {
	var zero sessionPayload
	dot := strings.IndexByte(value, '.')
	if dot <= 0 || dot == len(value)-1 {
		return zero, errors.New("malformed cookie")
	}
	b64, sigB64 := value[:dot], value[dot+1:]
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return zero, errors.New("malformed signature")
	}
	// hmac.Equal is a constant-time comparison (crypto/subtle under the hood);
	// it must be used rather than == to avoid a MAC timing side channel.
	if !hmac.Equal(sig, m.sign(b64)) {
		return zero, errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return zero, errors.New("malformed payload")
	}
	var p sessionPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return zero, errors.New("malformed payload json")
	}
	return p, nil
}

func (m *SessionManager) sign(msg string) []byte {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// LoadSessionKey reads the HMAC session key from a file (raw bytes). The file
// must contain at least minSessionKeyLen bytes.
func LoadSessionKey(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth.LoadSessionKey: read %s: %w", path, err)
	}
	if len(body) < minSessionKeyLen {
		return nil, fmt.Errorf("auth.LoadSessionKey: key is %d bytes, want >= %d", len(body), minSessionKeyLen)
	}
	return body, nil
}
