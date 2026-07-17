package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termsession"
)

type blackboxPTY struct {
	dead     chan struct{}
	killOnce sync.Once
	writes   atomic.Int32
	resizes  atomic.Int32
	written  chan struct{}
}

func newBlackboxPTY() *blackboxPTY {
	return &blackboxPTY{dead: make(chan struct{}), written: make(chan struct{}, 1)}
}

func (p *blackboxPTY) Read([]byte) (int, error) {
	<-p.dead
	return 0, io.EOF
}

func (p *blackboxPTY) Write(b []byte) (int, error) {
	p.writes.Add(1)
	select {
	case p.written <- struct{}{}:
	default:
	}
	return len(b), nil
}

func (p *blackboxPTY) Resize(uint16, uint16) error {
	p.resizes.Add(1)
	return nil
}

func (p *blackboxPTY) Wait() (int, error) {
	<-p.dead
	return 0, nil
}

func (p *blackboxPTY) Kill() error {
	p.killOnce.Do(func() { close(p.dead) })
	return nil
}

func (p *blackboxPTY) Close() error { return nil }

type blackboxSpawner struct {
	mu   sync.Mutex
	last *blackboxPTY
}

func (s *blackboxSpawner) Spawn(termsession.Spec) (termsession.PTY, error) {
	pty := newBlackboxPTY()
	s.mu.Lock()
	s.last = pty
	s.mu.Unlock()
	return pty, nil
}

func (s *blackboxSpawner) lastPTY(t *testing.T) *blackboxPTY {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		t.Fatal("spawner has no PTY")
	}
	return s.last
}

type remoteLaunchBlackboxHarness struct {
	s       *Server
	h       http.Handler
	ts      *httptest.Server
	rc      *remoteController
	adapter *realManagerAdapter
	mgr     *termsession.Manager
	handle  string
	pty     *blackboxPTY
	raw     string
	capTok  string
	confirm string
}

func newRemoteLaunchBlackboxHarness(t *testing.T) *remoteLaunchBlackboxHarness {
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

	spawner := &blackboxSpawner{}
	mgr := termsession.NewManager(termsession.Options{Spawner: spawner, ReapInterval: time.Hour})
	t.Cleanup(mgr.Shutdown)
	adapter := &realManagerAdapter{mgr: mgr, sess: rc, caps: rc}

	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc, LaunchManager: adapter})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	loopback := s.Handler()
	ck, token := getConfirm(t, loopback)
	if rec := postConfirm(t, loopback, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable remote = %d: %s", rec.Code, rec.Body.String())
	}

	handle, err := mgr.Create(termsession.Spec{Fresh: true, BinPath: "x", Subcommand: "claude"})
	if err != nil {
		t.Fatalf("mgr.Create: %v", err)
	}
	pty := spawner.lastPTY(t)

	raw, err := rc.sessions.Create()
	if err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	capTok, confirm, err := rc.MintTerminalControl(remoteauth.HashSessionID(raw), handle)
	if err != nil {
		t.Fatalf("MintTerminalControl: %v", err)
	}

	ts := httptest.NewServer(s.remoteBackendHandler(rc))
	t.Cleanup(ts.Close)

	return &remoteLaunchBlackboxHarness{
		s: s, h: loopback, ts: ts, rc: rc, adapter: adapter, mgr: mgr,
		handle: handle, pty: pty, raw: raw, capTok: capTok, confirm: confirm,
	}
}

func (h *remoteLaunchBlackboxHarness) dialWS(t *testing.T, raw string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	base := "ws" + strings.TrimPrefix(h.ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	header := http.Header{"Origin": {"http://" + testRemoteHost}}
	if raw != "" {
		header.Set("Cookie", remoteSessionCookie+"="+raw)
	}
	return websocket.Dial(ctx, base+"/ws/launch/"+h.handle, &websocket.DialOptions{
		Host:       testRemoteHost,
		HTTPHeader: header,
	})
}

func assertNoPTYSideEffect(t *testing.T, pty *blackboxPTY, writes, resizes int32) {
	t.Helper()
	if got := pty.writes.Load(); got != writes {
		t.Fatalf("PTY writes = %d, want %d", got, writes)
	}
	if got := pty.resizes.Load(); got != resizes {
		t.Fatalf("PTY resizes = %d, want %d", got, resizes)
	}
}

// TestRemoteGuardedLaunchWSViewThenAcquire drives /ws/launch through the real
// remote-exposed backend chain: browserGuard -> remoteAuthz -> route
// capability check -> launch websocket. It pins the split where the upgrade is
// View-tier, while PTY input remains gated by in-band terminal-control acquire.
func TestRemoteGuardedLaunchWSViewThenAcquire(t *testing.T) {
	h := newRemoteLaunchBlackboxHarness(t)

	c, _, err := h.dialWS(t, h.raw)
	if err != nil {
		t.Fatalf("view device should reach /ws/launch upgrade through remoteBackendHandler: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })

	anon, resp, err := h.dialWS(t, "")
	if err == nil {
		_ = anon.CloseNow()
		t.Fatal("anonymous /ws/launch dial unexpectedly upgraded")
	}
	if resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		if resp == nil {
			t.Fatal("anonymous /ws/launch dial failed without an HTTP response")
		}
		t.Fatalf("anonymous /ws/launch status = %d, want 401 or 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageBinary, []byte("early-keystroke"))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":40,"cols":120}`))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"bogus-control"}`))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"oob","data":"forged"}`))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"bogus","confirm":"bogus"}`))
	if !waitForControl(t, ctx, c, "control_denied") {
		t.Fatal("expected control_denied for bogus acquire")
	}
	assertNoPTYSideEffect(t, h.pty, 0, 0)
	if h.adapter.lastLease() != nil {
		t.Fatal("bogus acquire installed a remote writer lease")
	}

	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"`+h.capTok+`","confirm":"`+h.confirm+`"}`))
	if !waitForControl(t, ctx, c, "control_granted") {
		t.Fatal("expected control_granted for valid acquire after bogus attempt")
	}
	if h.adapter.lastLease() == nil {
		t.Fatal("valid acquire installed no remote writer lease")
	}

	_ = c.Write(ctx, websocket.MessageBinary, []byte("ls\n"))
	select {
	case <-h.pty.written:
	case <-time.After(2 * time.Second):
		t.Fatal("keystroke after valid acquire never reached the PTY")
	}
	writesAfterGrant := h.pty.writes.Load()
	resizesAfterGrant := h.pty.resizes.Load()

	if n := h.adapter.RevokeAllRemoteWriters("blackbox revoke"); n != 1 {
		t.Fatalf("RevokeAllRemoteWriters revoked %d writers, want 1", n)
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("after-revoke"))
	assertSocketClosed(t, c)
	assertNoPTYSideEffect(t, h.pty, writesAfterGrant, resizesAfterGrant)
}
