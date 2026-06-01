package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestRequireSAMLSession(t *testing.T) {
	sessions := newTestSessions(t)

	// Authenticated request: cookie present → handler runs, user injected.
	var sawUser string
	guarded := RequireSAMLSession(sessions, JSONUnauthorized())(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			sawUser, _ = UserIDFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		},
	))
	req := roundTrip(t, sessions, "user-7")
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || sawUser != "user-7" {
		t.Fatalf("authenticated: code=%d user=%q", rec.Code, sawUser)
	}

	// No cookie → JSON 401.
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/org/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: code=%d, want 401", rec.Code)
	}

	// Redirect mode for browser routes.
	redir := RequireSAMLSession(sessions, RedirectToSSO("/saml/sso"))(okHandler())
	rec = httptest.NewRecorder()
	redir.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/saml/sso" {
		t.Errorf("redirect mode: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// fakeVerifier implements BearerVerifier.
type fakeVerifier struct {
	claims orgcontract.BearerClaims
	err    error
}

func (f fakeVerifier) VerifyBearer(_ context.Context, _ string) (orgcontract.BearerClaims, error) {
	return f.claims, f.err
}

func TestRequireBearer(t *testing.T) {
	want := orgcontract.BearerClaims{Sub: "user-9", Aud: "org-1"}
	mw := RequireBearer(fakeVerifier{claims: want})

	var got orgcontract.BearerClaims
	guarded := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/agent/push", nil)
	req.Header.Set("Authorization", "Bearer sometoken")
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got.Sub != "user-9" {
		t.Fatalf("valid bearer: code=%d sub=%q", rec.Code, got.Sub)
	}

	// Missing header → 401.
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agent/push", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer: code=%d, want 401", rec.Code)
	}

	// Verifier rejects → 401.
	bad := RequireBearer(fakeVerifier{err: errors.New("revoked")})(okHandler())
	req = httptest.NewRequest(http.MethodPost, "/api/agent/push", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec = httptest.NewRecorder()
	bad.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("rejected bearer: code=%d, want 401", rec.Code)
	}
}

func TestRequireSCIMToken(t *testing.T) {
	guarded := RequireSCIMToken("s3cret-scim-token")(okHandler())

	// Correct token.
	req := httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
	req.Header.Set("Authorization", "Bearer s3cret-scim-token")
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("correct token: code=%d, want 200", rec.Code)
	}

	// Wrong token.
	req = httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: code=%d, want 401", rec.Code)
	}

	// No header.
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: code=%d, want 401", rec.Code)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
		ok     bool
	}{
		"standard":         {"Bearer abc", "abc", true},
		"lowercase scheme": {"bearer abc", "abc", true},
		"mixed case":       {"BeArEr abc", "abc", true},
		"trailing space":   {"Bearer  abc  ", "abc", true},
		"empty":            {"", "", false},
		"no scheme":        {"abc", "", false},
		"scheme only":      {"Bearer ", "", false},
		"wrong scheme":     {"Basic abc", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			got, ok := bearerToken(r)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWriteErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnauthorized, "unauthorized", "nope")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error":"unauthorized"`) || !strings.Contains(body, `"message":"nope"`) {
		t.Errorf("body = %s", body)
	}
}
