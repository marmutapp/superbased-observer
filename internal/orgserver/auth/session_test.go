package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestSessions(t *testing.T) *SessionManager {
	t.Helper()
	key := []byte(strings.Repeat("k", 32))
	m, err := NewSessionManager(key, time.Hour, true)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	return m
}

// roundTrip issues a cookie on a recorder and replays it on a fresh request.
func roundTrip(t *testing.T, m *SessionManager, userID string) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := m.Issue(rec, userID); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func TestSessionRoundTrip(t *testing.T) {
	m := newTestSessions(t)
	req := roundTrip(t, m, "user-42")
	got, err := m.UserID(req)
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	if got != "user-42" {
		t.Errorf("UserID = %q, want user-42", got)
	}
}

func TestNoCookieIsNoSession(t *testing.T) {
	m := newTestSessions(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := m.UserID(req); !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestTamperedCookieRejected(t *testing.T) {
	m := newTestSessions(t)
	req := roundTrip(t, m, "user-42")
	c, _ := req.Cookie(DefaultSessionCookie)
	// Flip a character in the signed body.
	bad := c.Value
	if bad[0] == 'A' {
		bad = "B" + bad[1:]
	} else {
		bad = "A" + bad[1:]
	}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: DefaultSessionCookie, Value: bad})
	if _, err := m.UserID(req2); !errors.Is(err, ErrNoSession) {
		t.Errorf("tampered: err = %v, want ErrNoSession", err)
	}
}

func TestSessionWrongKeyRejected(t *testing.T) {
	m := newTestSessions(t)
	req := roundTrip(t, m, "user-42")
	c, _ := req.Cookie(DefaultSessionCookie)

	other, _ := NewSessionManager([]byte(strings.Repeat("x", 32)), time.Hour, true)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(c)
	if _, err := other.UserID(req2); !errors.Is(err, ErrNoSession) {
		t.Errorf("cross-key: err = %v, want ErrNoSession", err)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	m := newTestSessions(t)
	m.now = func() time.Time { return time.Unix(1000, 0) }
	req := roundTrip(t, m, "user-42")
	// Advance well past the 1h TTL.
	m.now = func() time.Time { return time.Unix(1000+3601, 0) }
	if _, err := m.UserID(req); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired: err = %v, want ErrNoSession", err)
	}
}

func TestClearExpiresCookie(t *testing.T) {
	m := newTestSessions(t)
	rec := httptest.NewRecorder()
	m.Clear(rec)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Errorf("Clear did not emit an expiring cookie: %+v", cookies)
	}
}

func TestSecureAttribute(t *testing.T) {
	m := newTestSessions(t) // secure=true
	rec := httptest.NewRecorder()
	_ = m.Issue(rec, "u")
	if !rec.Result().Cookies()[0].Secure {
		t.Error("expected Secure cookie when secure=true")
	}
	if !rec.Result().Cookies()[0].HttpOnly {
		t.Error("expected HttpOnly cookie")
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := NewSessionManager([]byte("short"), time.Hour, false); err == nil {
		t.Error("expected error for short session key")
	}
}
