package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Middleware is the standard net/http middleware shape used throughout the
// org server: it wraps a handler and returns a handler.
type Middleware func(http.Handler) http.Handler

// ctxKey is the unexported context-key type for values this package injects.
type ctxKey int

const (
	ctxKeyUserID ctxKey = iota // SAML session subject (string)
	ctxKeyClaims               // validated bearer claims (orgcontract.BearerClaims)
)

// UserIDFromContext returns the SAML-authenticated user id placed by
// RequireSAMLSession, if any.
func UserIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUserID).(string)
	return v, ok
}

// ClaimsFromContext returns the bearer claims placed by RequireBearer, if any.
func ClaimsFromContext(ctx context.Context) (orgcontract.BearerClaims, bool) {
	v, ok := ctx.Value(ctxKeyClaims).(orgcontract.BearerClaims)
	return v, ok
}

// ContextWithUserID returns ctx carrying the SAML-authenticated user id,
// retrievable via UserIDFromContext. RequireSAMLSession uses it after a valid
// session; it is also the seam dashboard handler tests use to build an
// authenticated context without standing up the middleware chain.
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, userID)
}

// ContextWithClaims returns ctx carrying claims, retrievable via
// ClaimsFromContext. RequireBearer uses it after a successful validation; it is
// also the seam handler tests use to construct an authenticated context without
// standing up the middleware chain.
func ContextWithClaims(ctx context.Context, claims orgcontract.BearerClaims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims, claims)
}

// BearerVerifier validates a raw bearer end to end: signature + expiry
// (stateless) plus the DB-backed checks (jti not revoked, subject still
// exists and is active). Implemented by the server layer; kept an interface
// so this package does not import the DB.
type BearerVerifier interface {
	VerifyBearer(ctx context.Context, raw string) (orgcontract.BearerClaims, error)
}

// RequireSAMLSession guards dashboard/admin routes. On a valid session it
// injects the user id into the context; otherwise it invokes onUnauthorized,
// which differs by route group: a redirect to SSO for browser routes, a 401
// JSON for /api/org/* routes. (See RedirectToSSO and JSONUnauthorized.)
func RequireSAMLSession(sessions *SessionManager, onUnauthorized http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, err := sessions.UserID(r)
			if err != nil {
				onUnauthorized(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithUserID(r.Context(), userID)))
		})
	}
}

// RequireBearer guards the agent push route. It extracts the Authorization
// bearer, validates it through the verifier, and injects the claims.
func RequireBearer(v BearerVerifier) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			claims, err := v.VerifyBearer(r.Context(), raw)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid or revoked bearer")
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithClaims(r.Context(), claims)))
		})
	}
}

// RequireSCIMToken guards /scim/v2/*. It compares the presented bearer to the
// static SCIM token in constant time.
//
// The token is compared with crypto/subtle directly rather than argon2id:
// the IdP presents this token on every SCIM request, and per-request argon2
// (m=64MB, t=3) would be a denial-of-service vector. Constant-time raw
// comparison closes the timing side channel without the cost. argon2id is
// reserved for the at-rest enrolment-token hashes, where the cost is paid
// once at enrol time (spec §2.7's "SCIM API token comparison" is satisfied
// by constant-time comparison; the hashing emphasis is on token storage).
func RequireSCIMToken(expected string) Middleware {
	expectedB := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok || subtle.ConstantTimeCompare([]byte(raw), expectedB) != 1 {
				WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid SCIM token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RedirectToSSO returns an onUnauthorized handler that 302-redirects to the
// SP-initiated SSO endpoint. Used for browser-facing routes.
func RedirectToSSO(ssoPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ssoPath, http.StatusFound)
	}
}

// JSONUnauthorized returns an onUnauthorized handler that writes a 401 JSON
// error. Used for API routes under SAML auth (/api/org/*).
func JSONUnauthorized() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusUnauthorized, "unauthorized", "login required")
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. The scheme match is case-insensitive per RFC 7235.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// WriteError writes a JSON error matching the OpenAPI Error schema
// ({error, message}). Shared by the middlewares and the API handlers.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
