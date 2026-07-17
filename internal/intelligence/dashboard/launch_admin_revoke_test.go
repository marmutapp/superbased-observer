package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termlease"
	"github.com/marmutapp/superbased-observer/internal/termsession"
)

// launch_admin_revoke_test.go is the Phase-3a gate: it drives the REAL dashboard
// admin transitions (remote disable / rotate / device-session revoke /
// allow_terminal→false) and the REAL local takeover against a REAL
// termsession.Manager through a thin delegating adapter — the same shape as
// cmd's launchManagerAdapter — over a REAL websocket bridge, and proves:
//
//   - each admin transition terminates the LIVE remote writer lease through the
//     ONE termsession revocation funnel (Revoked() closes, a later Write is
//     fenced out with ErrNotWriter — generation advanced),
//   - the admin/device-invalidation kills CLOSE the remote socket (the device is
//     no longer trusted), while a LOCAL TAKEOVER only DEMOTES the remote to a
//     read-only viewer (socket stays open, control_revoked sent),
//   - the owner-LOCAL loopback writer is never touched.
//
// The test file imports termsession only for the fake PTY + the real Manager
// (test-only); the production dashboard package still carries no termsession
// dependency (the seam is the LaunchManager interface).

// --- fake PTY/spawner that keeps a session alive until Kill ---

type adminFakePTY struct {
	dead     chan struct{}
	killOnce sync.Once
}

func newAdminFakePTY() *adminFakePTY { return &adminFakePTY{dead: make(chan struct{})} }

func (p *adminFakePTY) Read(b []byte) (int, error)  { <-p.dead; return 0, io.EOF }
func (p *adminFakePTY) Write(b []byte) (int, error) { return len(b), nil }
func (p *adminFakePTY) Resize(uint16, uint16) error { return nil }
func (p *adminFakePTY) Wait() (int, error)          { <-p.dead; return 0, nil }
func (p *adminFakePTY) Kill() error                 { p.killOnce.Do(func() { close(p.dead) }); return nil }
func (p *adminFakePTY) Close() error                { return nil }

type adminFakeSpawner struct{}

func (adminFakeSpawner) Spawn(termsession.Spec) (termsession.PTY, error) {
	return newAdminFakePTY(), nil
}

// allowAllLaunchPolicy admits any handle — the launch-policy leg of §4.δ (the
// run was policy-checked at spawn; here we isolate the revoke behaviour).
type allowAllLaunchPolicy struct{}

func (allowAllLaunchPolicy) Allowed(string) bool { return true }

// realManagerAdapter delegates dashboard.LaunchManager to a REAL
// termsession.Manager, running the SAME §4.δ authorize → mgr.AcquireWriterRemote
// path the cmd adapter runs. It captures the last remote lease so the test can
// assert the manager-side fence directly.
type realManagerAdapter struct {
	mgr    *termsession.Manager
	sess   termlease.SessionValidator
	caps   termlease.CapabilityConsumer
	lastMu sync.Mutex
	last   *termsession.WriterLease
}

func (a *realManagerAdapter) Create(spec LaunchSpec) (string, error) {
	return a.mgr.Create(termsession.Spec{Fresh: true, BinPath: "x", Subcommand: spec.Subcommand})
}

func (a *realManagerAdapter) CreateFresh(spec FreshLaunchSpec) (string, error) {
	return a.mgr.Create(termsession.Spec{Fresh: true, BinPath: "x", Subcommand: spec.Subcommand})
}
func (a *realManagerAdapter) CreateSetup(SetupSpec) (string, error) { return "", nil }
func (a *realManagerAdapter) Subscribe(handle string) (LaunchSubscription, error) {
	return a.mgr.Subscribe(handle)
}

func (a *realManagerAdapter) SubscribeRemote(handle string) (LaunchSubscription, error) {
	sub, err := a.mgr.SubscribeRemote(handle)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (a *realManagerAdapter) IsSetupSession(handle string) bool {
	return a.mgr.IsSetupSession(handle)
}

func (a *realManagerAdapter) Unsubscribe(sub LaunchSubscription) {
	if ts, ok := sub.(*termsession.Subscription); ok {
		a.mgr.Unsubscribe(ts)
	}
}

func (a *realManagerAdapter) AcquireWriterLocal(handle string) (LaunchWriter, error) {
	return a.mgr.AcquireWriterLocal(handle)
}

func (a *realManagerAdapter) AcquireWriterRemote(req RemoteWriterRequest) (LaunchWriter, error) {
	grant, err := termlease.Authorize(termlease.AuthorizeRequest{
		Handle:          req.Handle,
		DeviceSessionID: req.DeviceSessionID,
		CapabilityToken: req.CapabilityToken,
		Confirm:         req.Confirm,
		RemoteExposed:   req.RemoteExposed,
		AllowTerminal:   true,
	}, a.sess, allowAllLaunchPolicy{}, a.caps)
	if err != nil {
		return nil, err
	}
	l, err := a.mgr.AcquireWriterRemote(req.Handle, grant)
	if err != nil {
		return nil, err
	}
	a.lastMu.Lock()
	a.last = l
	a.lastMu.Unlock()
	return l, nil
}
func (a *realManagerAdapter) Close(handle string) { a.mgr.Close(handle) }
func (a *realManagerAdapter) Snapshot() []LaunchInfo {
	var out []LaunchInfo
	for _, s := range a.mgr.Snapshot() {
		out = append(out, LaunchInfo{ID: s.ID, WriterHolder: s.WriterHolder})
	}
	return out
}

func (a *realManagerAdapter) RevokeAllRemoteWriters(reason string) int {
	return a.mgr.RevokeAllRemoteWriters(reason)
}

func (a *realManagerAdapter) RevokeRemoteWriterByHolder(fp, reason string) bool {
	return a.mgr.RevokeRemoteWriterByHolder(fp, reason)
}

func (a *realManagerAdapter) lastLease() *termsession.WriterLease {
	a.lastMu.Lock()
	defer a.lastMu.Unlock()
	return a.last
}

// --- harness ---

type adminRevokeHarness struct {
	s       *Server
	h       http.Handler // loopback handler (Local management routes reachable)
	ts      *httptest.Server
	rc      *remoteController
	adapter *realManagerAdapter
	mgr     *termsession.Manager
	handle  string
	ck      *http.Cookie
	token   string
}

func newAdminRevokeHarness(t *testing.T) *adminRevokeHarness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[observer]\ndb_path = \""+filepath.ToSlash(dbPath)+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	rcAny, _ := newReadyRemoteController(t)
	rc := rcAny.(*remoteController)

	mgr := termsession.NewManager(termsession.Options{Spawner: adminFakeSpawner{}, ReapInterval: time.Hour})
	t.Cleanup(mgr.Shutdown)
	adapter := &realManagerAdapter{mgr: mgr, sess: rc, caps: rc}

	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc, LaunchManager: adapter})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hh := s.Handler()
	ts := remoteExposedWSServer(t, s)
	t.Cleanup(ts.Close)

	// Spawn a live terminal session directly on the manager.
	handle, err := mgr.Create(termsession.Spec{Fresh: true, BinPath: "x", Subcommand: "claude"})
	if err != nil {
		t.Fatalf("mgr.Create: %v", err)
	}

	h := &adminRevokeHarness{s: s, h: hh, ts: ts, rc: rc, adapter: adapter, mgr: mgr, handle: handle}

	// Arm remote so rotate/enable have an enabled config to mint against, and set
	// the confirm token the management POSTs echo.
	h.ck, h.token = getConfirm(t, hh)
	if rec := postConfirm(t, hh, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal":true}`, h.ck, h.token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	// Refresh the confirm token after the enable write (getConfirm mints one per
	// GET; reuse the same one — it is still valid for subsequent POSTs).
	return h
}

// pairAndAcquire pairs a device on the controller, opens a remote-exposed WS
// bridge carrying its cookie, acquires the remote writer lease through the REAL
// §4.δ conjunction, and returns the WS conn, the raw cookie, and the device
// fingerprint (= grant.Holder() = /api/remote/sessions fingerprint).
func (h *adminRevokeHarness) pairAndAcquire(t *testing.T) (*websocket.Conn, string, string) {
	t.Helper()
	raw, err := h.rc.sessions.Create()
	if err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	fp := ""
	for _, si := range h.rc.Sessions() {
		if si.ID == remoteauth.HashSessionID(raw) {
			fp = si.Fingerprint
		}
	}
	if fp == "" {
		t.Fatal("paired session has no fingerprint")
	}
	// Mint a single-use terminal-control capability for (device hash, handle).
	tok, confirm, err := h.rc.MintTerminalControl(remoteauth.HashSessionID(raw), h.handle)
	if err != nil {
		t.Fatalf("MintTerminalControl: %v", err)
	}

	base := "ws" + trimHTTP(h.ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/"+h.handle, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin": {h.ts.URL},
			"Cookie": {remoteSessionCookie + "=" + raw},
		},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	_ = c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"t":"acquire-writer","cap":"`+tok+`","confirm":"`+confirm+`"}`))
	if !waitForControl(t, context.Background(), c, "control_granted") {
		t.Fatal("expected control_granted after a valid remote acquire")
	}
	if h.adapter.lastLease() == nil {
		t.Fatal("adapter captured no remote lease")
	}
	if got := h.adapter.lastLease().Holder(); got != fp {
		t.Fatalf("lease holder %q != device fingerprint %q — the unified fingerprint is broken", got, fp)
	}
	return c, raw, fp
}

func trimHTTP(url string) string {
	if len(url) >= 4 && url[:4] == "http" {
		return url[4:]
	}
	return url
}

// assertLeaseDeadAndFenced proves the captured lease was revoked through the
// funnel: its channel is closed and a subsequent Write is fenced (ErrNotWriter,
// generation advanced), so a stale remote Write can never reach the PTY.
func (h *adminRevokeHarness) assertLeaseDeadAndFenced(t *testing.T) {
	t.Helper()
	l := h.adapter.lastLease()
	select {
	case <-l.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("remote writer lease was NOT revoked by the admin transition")
	}
	if _, err := l.Write([]byte("stale")); err == nil {
		t.Fatal("a stale remote Write reached the PTY after revocation — the generation fence failed")
	}
}

// assertSocketClosed proves the remote websocket was closed by the bridge
// (admin/device-invalidation kill). The client should observe a control_revoked
// then a close/read error within the deadline.
func assertSocketClosed(t *testing.T, c *websocket.Conn) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			return // socket closed / read errored — the bridge tore it down
		}
	}
	t.Fatal("remote websocket stayed OPEN after an admin/device-invalidation kill — it must be closed")
}

// assertSocketStillOpen proves the socket survives (a local takeover only
// demotes): after receiving control_revoked, a subsequent read blocks (idle
// PTY) and times out rather than returning a websocket CLOSE — the bridge did
// NOT tear the socket down.
func assertSocketStillOpen(t *testing.T, c *websocket.Conn) {
	t.Helper()
	if !waitForControl(t, context.Background(), c, "control_revoked") {
		t.Fatal("expected control_revoked on a local takeover (demote)")
	}
	// A short read: on a demoted-but-open socket the idle PTY produces no
	// frames, so Read times out (CloseStatus == -1). A closed socket would yield
	// a close frame (CloseStatus >= 0) or a connection error.
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	_, _, err := c.Read(ctx)
	if err == nil {
		return // an output frame arrived — the socket is alive
	}
	if websocket.CloseStatus(err) != -1 {
		t.Fatalf("socket was CLOSED on a local takeover (close status %v) — a takeover must only DEMOTE", websocket.CloseStatus(err))
	}
	if ctx.Err() == nil {
		t.Fatalf("unexpected read error on a demoted socket (not a timeout, not a close): %v", err)
	}
}

// --- the four admin-close gate tests + the takeover-demote contrast ---

func TestDisableClosesOpenWriterSocketAndRevokesLease(t *testing.T) {
	h := newAdminRevokeHarness(t)
	c, _, _ := h.pairAndAcquire(t)
	rec := postConfirm(t, h.h, "/api/remote/disable", `{}`, h.ck, h.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}
	h.assertLeaseDeadAndFenced(t)
	assertSocketClosed(t, c)
}

func TestRotateClosesOpenWriterSocketAndRevokesLease(t *testing.T) {
	h := newAdminRevokeHarness(t)
	c, _, _ := h.pairAndAcquire(t)
	rec := postConfirm(t, h.h, "/api/remote/rotate", `{}`, h.ck, h.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body.String())
	}
	h.assertLeaseDeadAndFenced(t)
	assertSocketClosed(t, c)
}

func TestDeviceRevokeClosesOpenWriterSocketAndRevokesLease(t *testing.T) {
	h := newAdminRevokeHarness(t)
	c, _, fp := h.pairAndAcquire(t)
	// DELETE /api/remote/sessions/<fp> — no confirm token required (method gate).
	req := httptest.NewRequest(http.MethodDelete, "/api/remote/sessions/"+fp, nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("device revoke = %d: %s", rec.Code, rec.Body.String())
	}
	h.assertLeaseDeadAndFenced(t)
	assertSocketClosed(t, c)
}

func TestAllowTerminalFalseClosesOpenWriterSocketAndRevokesLease(t *testing.T) {
	h := newAdminRevokeHarness(t)
	c, _, _ := h.pairAndAcquire(t)
	// Re-arm with allow_terminal=false → allow_terminal→false transition. Device
	// sessions persist (cookie-based), so the lease's device stays valid; only
	// the writer lease dies.
	rec := postConfirm(t, h.h, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal":false}`, h.ck, h.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable(allow_terminal=false) = %d: %s", rec.Code, rec.Body.String())
	}
	h.assertLeaseDeadAndFenced(t)
	assertSocketClosed(t, c)
}

func TestLocalTakeoverDemotesRemoteWriterButKeepsSocket(t *testing.T) {
	h := newAdminRevokeHarness(t)
	c, _, _ := h.pairAndAcquire(t)

	// A LOCAL takeover (the owner-local loopback path) revokes the remote lease
	// but the device session stays valid → the bridge must only DEMOTE.
	if _, err := h.mgr.AcquireWriterLocal(h.handle); err != nil {
		t.Fatalf("local takeover: %v", err)
	}
	// The remote lease is fenced (demoted): a stale remote Write is dropped.
	h.assertLeaseDeadAndFenced(t)
	// But the socket stays open as a read-only viewer.
	assertSocketStillOpen(t, c)
}
