// Package dashboard — Phase-4 session-attach round-2 review regression tests
// (F1 session-lifetime binding, F2 full-hash viewer keying + logout gating, F4
// strict terminal-settings decode).
package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// fakeLifetimer is a controllable deviceSessionLifetimer for the F1b watch-core
// tests: it returns a fixed revocation channel + until + liveness, swappable
// mid-flight so a TTL expiry can be simulated between polls.
type fakeLifetimer struct {
	mu      sync.Mutex
	revoked <-chan struct{}
	until   time.Duration
	live    bool
}

func (f *fakeLifetimer) set(live bool) {
	f.mu.Lock()
	f.live = live
	f.mu.Unlock()
}

func (f *fakeLifetimer) SessionLifetime(string) (<-chan struct{}, time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked, f.until, f.live
}

// TestDeviceSessionLiveF1a pins the F1a post-register re-validation: a live
// device session is admitted, a revoked one is refused, and the loopback / empty
// cases are treated as "not disproven" (true) so existing no-session view paths
// are unchanged.
func TestDeviceSessionLiveF1a(t *testing.T) {
	rc := NewRemoteController(RemoteOptions{})
	mc := rc.(*remoteController)
	raw, err := mc.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s := &Server{opts: Options{Remote: rc}}

	if !s.deviceSessionLive(raw) {
		t.Fatal("a live device session must pass the F1a re-validation")
	}
	// Revoke mid-handshake: the post-register re-read must refuse.
	if err := mc.sessions.RevokeByHash(remoteauth.HashSessionID(raw)); err != nil {
		t.Fatalf("RevokeByHash: %v", err)
	}
	if s.deviceSessionLive(raw) {
		t.Fatal("a revoked device session must be refused by the F1a re-validation")
	}
	// Empty cookie + no controller are both "not disproven".
	if !s.deviceSessionLive("") {
		t.Error("empty cookie must be treated as live (nothing to re-resolve)")
	}
	if !(&Server{}).deviceSessionLive(raw) {
		t.Error("no controller (loopback) must be treated as live")
	}
}

// TestWatchSessionLifetimeRevoke pins F1b: a bound viewer is cancelled the moment
// its session's revocation channel closes (revoke / rotate).
func TestWatchSessionLifetimeRevoke(t *testing.T) {
	ch := make(chan struct{})
	lt := &fakeLifetimer{revoked: ch, until: time.Hour, live: true}
	canceled := make(chan struct{})
	done := make(chan struct{})
	go watchSessionLifetime("raw", lt, done, func() { close(canceled) }, 5*time.Millisecond)

	close(ch) // session revoked/rotated
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("viewer not cancelled when the session revocation channel closed")
	}
}

// TestWatchSessionLifetimeExpiry pins F1b: a bound viewer is cancelled on TTL/idle
// expiry — the timer fires, the re-read reports live=false, and cancel runs.
func TestWatchSessionLifetimeExpiry(t *testing.T) {
	ch := make(chan struct{}) // never closed — expiry, not revoke, is the trigger
	lt := &fakeLifetimer{revoked: ch, until: 10 * time.Millisecond, live: true}
	canceled := make(chan struct{})
	done := make(chan struct{})
	go watchSessionLifetime("raw", lt, done, func() { close(canceled) }, 5*time.Millisecond)

	// Flip to expired shortly after start; the next timer wake re-reads live=false.
	time.Sleep(15 * time.Millisecond)
	lt.set(false)
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("viewer not cancelled on TTL/idle expiry")
	}
}

// TestWatchSessionLifetimeDisconnect pins F1b: a clean viewer disconnect (done
// closed) ends the watch WITHOUT cancelling (the viewer already left).
func TestWatchSessionLifetimeDisconnect(t *testing.T) {
	ch := make(chan struct{})
	lt := &fakeLifetimer{revoked: ch, until: time.Hour, live: true}
	var canceledN int32
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		watchSessionLifetime("raw", lt, done, func() { atomic.AddInt32(&canceledN, 1) }, 5*time.Millisecond)
		close(finished)
	}()
	close(done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return on viewer disconnect")
	}
	if atomic.LoadInt32(&canceledN) != 0 {
		t.Error("a clean disconnect must NOT cancel the viewer")
	}
}

// TestWatchSessionLifetimeExpiryReal pins F1b end-to-end through the REAL
// controller/session-store: a short-TTL session expires and the bound viewer is
// cancelled — no revoke, purely the timer-less TTL path.
func TestWatchSessionLifetimeExpiryReal(t *testing.T) {
	rc := NewRemoteController(RemoteOptions{Session: remoteauth.SessionParams{TTL: 40 * time.Millisecond}})
	mc := rc.(*remoteController)
	raw, err := mc.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	canceled := make(chan struct{})
	done := make(chan struct{})
	go watchSessionLifetime(raw, mc, done, func() { close(canceled) }, 5*time.Millisecond)
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("viewer not cancelled on real TTL expiry")
	}
}

// TestSensitiveViewerKeyedByFullHashF2b pins F2b: the viewer registry is keyed by
// the FULL device-session hash — the 32-bit display fingerprint (hash[:8]) must
// NOT match a viewer registered under the full hash, so a prefix collision can
// never disconnect the wrong device.
func TestSensitiveViewerKeyedByFullHashF2b(t *testing.T) {
	s := &Server{}
	raw := "cookie-value"
	full := deviceSessionKey(raw)    // sha256 hex (registry key)
	prefix := deviceFingerprint(raw) // hash[:8] display token
	if full == prefix || !strings.HasPrefix(full, prefix) {
		t.Fatalf("expected fingerprint to be the display prefix of the full key (full=%q prefix=%q)", full, prefix)
	}

	var closed int32
	un := s.registerSensitiveViewer(full, func() { atomic.AddInt32(&closed, 1) })
	defer un()

	// The DISPLAY PREFIX must not target the full-hash-keyed viewer.
	if n := s.closeRemoteSensitiveViewersForDevice(prefix); n != 0 {
		t.Fatalf("display prefix closed %d viewers — it must be display-only, never a registry key", n)
	}
	if atomic.LoadInt32(&closed) != 0 {
		t.Fatal("viewer cancelled by a display-prefix collision")
	}
	// The FULL key targets it.
	if n := s.closeRemoteSensitiveViewersForDevice(full); n != 1 {
		t.Fatalf("full key closed %d viewers, want 1", n)
	}
}

// TestLogoutHookGatedOnLiveSessionF2a pins F2a: /api/remote/logout fires the
// session-revoke hook ONLY when a live session actually existed. An unknown-
// cookie logout closes nothing; a real logout closes exactly that device's
// full-hash-keyed viewer.
func TestLogoutHookGatedOnLiveSessionF2a(t *testing.T) {
	// newManageServer wires the controller's session-revoke hook to the Server's
	// per-device viewer close (New → SetSessionRevokeHook). The logout route
	// mounts only on the remote-guarded handler, so drive the controller handler
	// directly — it exercises the exact F2a gating + full-hash hook fire.
	s, _ := newManageServer(t)
	mc, ok := s.opts.Remote.(*remoteController)
	if !ok {
		t.Fatal("expected a concrete *remoteController")
	}
	raw, err := mc.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var closed int32
	un := s.registerSensitiveViewer(deviceSessionKey(raw), func() { atomic.AddInt32(&closed, 1) })
	defer un()

	// (1) Unknown-cookie logout: Revoke no-ops → hook must NOT fire.
	logout(t, mc, "totally-unknown-cookie-value")
	if atomic.LoadInt32(&closed) != 0 {
		t.Fatal("an unknown-cookie logout closed a viewer — the hook must be gated on a real revoke (F2a)")
	}

	// (2) Real logout: the live session is revoked → hook fires with the FULL
	// hash → this device's viewer is closed.
	logout(t, mc, raw)
	if atomic.LoadInt32(&closed) != 1 {
		t.Fatalf("real logout closed %d viewers, want 1 (F2a/F2b)", atomic.LoadInt32(&closed))
	}
}

// logout invokes the controller's handleLogout with the given session cookie.
func logout(t *testing.T, mc *remoteController, cookie string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/remote/logout", nil)
	req.Host = "127.0.0.1:8080"
	req.AddCookie(&http.Cookie{Name: remoteSessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	mc.handleLogout(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalSectionStrictDecodeF4 pins F4: the terminal Settings section
// rejects an unknown-only body (a typo) with 400 naming the accepted fields,
// rejects an all-omitted body, and still accepts a valid partial.
func TestTerminalSectionStrictDecodeF4(t *testing.T) {
	base := `[terminal.attach]
enabled = true
route_proxy = true
`
	// (1) Unknown-only body (typo) ⇒ 400 (no silent no-op "saved:true").
	rec := putTerminalSection(t, base, `{"RouteProxi":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown-only body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Enabled") || !strings.Contains(rec.Body.String(), "RouteProxy") {
		t.Errorf("400 body must name the accepted fields: %s", rec.Body.String())
	}
	// (2) Empty/all-omitted body ⇒ 400.
	if rec := putTerminalSection(t, base, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// (3) Valid partial still works.
	if rec := putTerminalSection(t, base, `{"RouteProxy":false}`); rec.Code != http.StatusOK {
		t.Fatalf("valid partial = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// putTerminalSection builds a fresh dashboard over a temp config with the given
// [terminal.attach] body and PUTs the given JSON to the terminal section.
func putTerminalSection(t *testing.T, cfgBody, jsonBody string) *httptest.ResponseRecorder {
	t.Helper()
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/terminal", strings.NewReader(jsonBody)))
	return rr
}
