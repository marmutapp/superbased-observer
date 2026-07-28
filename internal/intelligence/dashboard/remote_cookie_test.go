package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// pairDirect drives handlePair on the controller itself (no host guard / mux),
// so these cases stay off the slow assembled-Server path.
func pairDirect(t *testing.T, c *remoteController, enc string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair", strings.NewReader(`{"secret":"`+enc+`"}`))
	req.Host = testRemoteHost
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.handlePair(rec, req)
	return rec
}

func newPairableController(t *testing.T, sp remoteauth.SessionParams) (*remoteController, string) {
	t.Helper()
	raw, enc, err := remoteauth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	hash, err := remoteauth.HashSecret(raw)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	rc := NewRemoteController(RemoteOptions{
		HashedSecret:    hash,
		AllowedHosts:    []string{testRemoteHost},
		RateLimitPerMin: 60,
		Session:         sp,
	})
	c, ok := rc.(*remoteController)
	if !ok {
		t.Fatalf("NewRemoteController returned %T", rc)
	}
	return c, enc
}

func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == remoteSessionCookie {
			return ck
		}
	}
	return nil
}

// TestPairCookieIsPersistent is the regression pin for the "paired phone loses
// auth after ~1h" bug: the device cookie carried a durable 48h server session
// with NO Max-Age/Expires, making it a browser-SESSION cookie that mobile
// browsers discard when the tab is evicted. Every API call then 401'd behind a
// rendered shell.
//
// It also pins the security attributes in the SAME assertion, because the fix
// touches this exact Set-Cookie: weakening HttpOnly / Secure / SameSite=Strict
// while adding a lifetime would be a security regression, not a fix.
func TestPairCookieIsPersistent(t *testing.T) {
	cases := []struct {
		name       string
		ttl        time.Duration
		wantMaxAge int
	}{
		{"configured ttl", 90 * time.Minute, 5400},
		{"default ttl", 0, int(remoteauth.DefaultSessionTTL.Seconds())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, enc := newPairableController(t, remoteauth.SessionParams{TTL: tc.ttl, Idle: time.Hour, Max: 5})
			rec := pairDirect(t, c, enc)
			if rec.Code != http.StatusOK {
				t.Fatalf("pair: %d %s", rec.Code, rec.Body.String())
			}
			ck := sessionCookieFrom(t, rec)
			if ck == nil {
				t.Fatal("no session cookie set on pair")
			}
			if ck.MaxAge <= 0 {
				t.Fatalf("Max-Age = %d — a cookie with no positive Max-Age is a BROWSER-SESSION cookie and is dropped by mobile browsers within the hour (the bug)", ck.MaxAge)
			}
			if ck.MaxAge != tc.wantMaxAge {
				t.Errorf("Max-Age = %d, want %d (the session store's own absolute TTL)", ck.MaxAge, tc.wantMaxAge)
			}
			if !ck.HttpOnly {
				t.Error("HttpOnly lost — security regression")
			}
			if !ck.Secure {
				t.Error("Secure lost — security regression")
			}
			if ck.SameSite != http.SameSiteStrictMode {
				t.Errorf("SameSite = %v, want Strict — security regression", ck.SameSite)
			}
			if ck.Path != "/" {
				t.Errorf("Path = %q, want /", ck.Path)
			}
			// The raw header must actually carry the attribute (a Max-Age of 0
			// would be omitted entirely and silently restore the bug).
			if raw := rec.Result().Header.Get("Set-Cookie"); !strings.Contains(raw, "Max-Age=") {
				t.Errorf("Set-Cookie header has no Max-Age attribute: %q", raw)
			}
		})
	}
}

// TestPairCookieMaxAgeMatchesSessionLifetime pins the coupling: the cookie's
// lifetime is DERIVED from the session store, not a second hardcoded constant
// that can drift away from [remote].session_ttl_minutes.
func TestPairCookieMaxAgeMatchesSessionLifetime(t *testing.T) {
	c, enc := newPairableController(t, remoteauth.SessionParams{TTL: 3 * time.Hour, Idle: time.Hour, Max: 5})
	ck := sessionCookieFrom(t, pairDirect(t, c, enc))
	if ck == nil {
		t.Fatal("no session cookie")
	}
	if want := int(c.sessions.TTL().Seconds()); ck.MaxAge != want {
		t.Errorf("Max-Age %d != session TTL %d — the cookie must never expire before the session it carries", ck.MaxAge, want)
	}
}

// TestPairAtDeviceCapIsActionable pins the legible cap failure: at the device
// cap, pairing stays fail-CLOSED (no live session is evicted to make room) but
// says so in a machine-readable way instead of a bare 503 "cannot create
// session".
func TestPairAtDeviceCapIsActionable(t *testing.T) {
	c, enc := newPairableController(t, remoteauth.SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 1})
	if rec := pairDirect(t, c, enc); rec.Code != http.StatusOK {
		t.Fatalf("first pair: %d %s", rec.Code, rec.Body.String())
	}
	rec := pairDirect(t, c, enc)
	if rec.Code != http.StatusConflict {
		t.Fatalf("pair at cap = %d, want 409 Conflict (distinguishable from a generic failure)", rec.Code)
	}
	var body pairErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("pair-at-cap body is not JSON (%v): %s", err, rec.Body.String())
	}
	if body.Code != pairCodeTooManyDevices {
		t.Errorf("code = %q, want %q", body.Code, pairCodeTooManyDevices)
	}
	if !strings.Contains(strings.ToLower(body.Error), "revoke") {
		t.Errorf("error message must name the remedy (revoke a device), got %q", body.Error)
	}
	if ck := sessionCookieFrom(t, rec); ck != nil {
		t.Error("a failed pair must not set a session cookie")
	}
	// Fail-closed: the already-paired device is untouched.
	if got := c.sessions.Count(); got != 1 {
		t.Errorf("live sessions after a refused pair = %d, want the original 1 (never evict a live device)", got)
	}
}
