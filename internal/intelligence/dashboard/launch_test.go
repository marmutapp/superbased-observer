package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// --- fakes ---

type fakeLaunchSession struct {
	release chan struct{}
}

func newFakeLaunchSession() *fakeLaunchSession {
	return &fakeLaunchSession{release: make(chan struct{})}
}

func (f *fakeLaunchSession) Read(p []byte) (int, error)  { <-f.release; return 0, io.EOF }
func (f *fakeLaunchSession) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeLaunchSession) Resize(uint16, uint16) error { return nil }
func (f *fakeLaunchSession) Done() <-chan struct{}       { return f.release }
func (f *fakeLaunchSession) Exited() (bool, int)         { return false, 0 }

type fakeLaunchManager struct {
	sess      *fakeLaunchSession
	createErr error
	attachErr error
	lastSpec  LaunchSpec
}

func (m *fakeLaunchManager) Create(spec LaunchSpec) (string, error) {
	if m.createErr != nil {
		return "", m.createErr
	}
	m.lastSpec = spec
	return "HANDLE-abc", nil
}

func (m *fakeLaunchManager) Attach(handle string) (LaunchSession, error) {
	if m.attachErr != nil {
		return nil, m.attachErr
	}
	return m.sess, nil
}

func (m *fakeLaunchManager) Detach(string)                       {}
func (m *fakeLaunchManager) Resize(string, uint16, uint16) error { return nil }
func (m *fakeLaunchManager) Close(string)                        {}
func (m *fakeLaunchManager) Snapshot() []LaunchInfo              { return nil }

func newLaunchTestServer(t *testing.T, lm LaunchManager) *Server {
	t.Helper()
	tdir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, LaunchManager: lm})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func postSessionLaunch(t *testing.T, h http.Handler, sessionID, to string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(launchRequest{To: to})
	req := httptest.NewRequest(http.MethodPost, "/api/session/"+sessionID+"/launch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- POST /api/session/<id>/launch validation ---

func TestLaunchPOSTDisabledWhenNilManager(t *testing.T) {
	s := newLaunchTestServer(t, nil)
	rec := postSessionLaunch(t, s.Handler(), "sess-1", "claude-code")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil manager: status = %d, want 503", rec.Code)
	}
}

func TestLaunchPOSTRejectsUnlaunchableTool(t *testing.T) {
	s := newLaunchTestServer(t, &fakeLaunchManager{})
	// cline is the VS Code extension adapter — no CLI launcher, so no
	// LaunchSpec and not launchable in the embedded terminal (distinct from
	// cline-cli, which is). cursor is now launchable, so it no longer fits.
	rec := postSessionLaunch(t, s.Handler(), "sess-1", "cline")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unlaunchable tool: status = %d, want 400", rec.Code)
	}
}

func TestLaunchPOSTRejectsBadCarry(t *testing.T) {
	s := newLaunchTestServer(t, &fakeLaunchManager{})
	body, _ := json.Marshal(launchRequest{To: "claude-code", Carry: "bogus"})
	req := httptest.NewRequest(http.MethodPost, "/api/session/sess-1/launch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad carry: status = %d, want 400", rec.Code)
	}
}

func TestLaunchPOSTMintsHandle(t *testing.T) {
	lm := &fakeLaunchManager{}
	s := newLaunchTestServer(t, lm)
	rec := postSessionLaunch(t, s.Handler(), "sess-42", "codex")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out struct {
		Token      string `json:"token"`
		Subcommand string `json:"subcommand"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	handle := out.Token
	if handle == "" {
		t.Error("empty handle in response")
	}
	// The server derived the launcher subcommand from the capability
	// registry, not from the client — codex → "codex".
	if out.Subcommand != "codex" {
		t.Errorf("subcommand = %q, want codex", out.Subcommand)
	}
	if lm.lastSpec.SessionID != "sess-42" || lm.lastSpec.Subcommand != "codex" {
		t.Errorf("spec = %+v, want session sess-42 / subcommand codex", lm.lastSpec)
	}
}

// --- GET /api/launch/sessions + DELETE /api/launch/<handle> ---

func TestLaunchAdminListAndDelete(t *testing.T) {
	s := newLaunchTestServer(t, &fakeLaunchManager{})

	req := httptest.NewRequest(http.MethodGet, "/api/launch/sessions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sessions") {
		t.Errorf("list body missing sessions key: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/launch/some-handle", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
}

func TestLaunchAdminDisabledWhenNilManager(t *testing.T) {
	s := newLaunchTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/launch/sessions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil manager list status = %d, want 503", rec.Code)
	}
}

// --- GET /ws/launch/<handle> CSWSH protection ---

// TestLaunchWSRejectsCrossOrigin is the security-critical assertion: a
// websocket upgrade whose Origin host differs from the request Host is
// rejected by coder/websocket.Accept's default same-origin check — the CSWSH
// defense the whole launch surface leans on (together with the opaque handle
// minted only by the Origin-checked POST). A same-origin upgrade succeeds.
func TestLaunchWSRejectsCrossOrigin(t *testing.T) {
	lm := &fakeLaunchManager{sess: newFakeLaunchSession()}
	t.Cleanup(func() { close(lm.sess.release) })
	s := newLaunchTestServer(t, lm)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	handle := "HANDLE-abc"
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Cross-origin: must be rejected.
	if c, _, err := websocket.Dial(ctx, base+"/ws/launch/"+handle, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://evil.example"}},
	}); err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("cross-origin ws upgrade was accepted — CSWSH hole")
	}

	// Same-origin: must succeed.
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/"+handle, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("same-origin ws upgrade rejected: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "done")
}

func TestLaunchWSUnknownHandleClosed(t *testing.T) {
	// Attach fails → the upgrade is accepted (same-origin) then closed with
	// a policy-violation status; Dial itself succeeds at the handshake.
	lm := &fakeLaunchManager{attachErr: ErrLaunchAlreadyAttached}
	s := newLaunchTestServer(t, lm)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/launch/whatever", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("same-origin dial: %v", err)
	}
	// The server closes it; a read returns an error.
	if _, _, rerr := c.Read(ctx); rerr == nil {
		t.Error("expected the server to close the already-attached session")
	}
	c.CloseNow()
}
