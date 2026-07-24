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

// fakeSubscription is a read-only viewer whose Read blocks until release, then
// returns io.EOF — enough to keep a WS bridge alive for a handshake test.
type fakeSubscription struct {
	release chan struct{}
}

func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{release: make(chan struct{})}
}

func (f *fakeSubscription) Read(p []byte) (int, error) { <-f.release; return 0, io.EOF }
func (f *fakeSubscription) Done() <-chan struct{}      { return f.release }
func (f *fakeSubscription) Exited() (bool, int)        { return false, 0 }
func (f *fakeSubscription) Lost() int64                { return 0 }

// fakeWriter is a no-op writer lease.
type fakeWriter struct {
	revoked chan struct{}
}

func newFakeWriter() *fakeWriter { return &fakeWriter{revoked: make(chan struct{})} }

func (f *fakeWriter) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeWriter) Resize(uint16, uint16) error { return nil }
func (f *fakeWriter) Revoked() <-chan struct{}    { return f.revoked }
func (f *fakeWriter) Release()                    {}
func (f *fakeWriter) Holder() string              { return "local" }
func (f *fakeWriter) RevokeIsTakeover() bool      { return false }

type fakeLaunchManager struct {
	sub            *fakeSubscription
	createErr      error
	freshErr       error
	setupErr       error
	subscribeErr   error
	resumeErr      error
	lastSpec       LaunchSpec
	lastFreshSpec  FreshLaunchSpec
	lastResumeSpec ResumeLaunchSpec
	lastSetupSpec  SetupSpec
	// setupHandles marks which handles IsSetupSession reports true for. Nil ⇒
	// none (unchanged default), so existing tests see no setup sessions.
	setupHandles map[string]bool
	// attachHandles marks which handles IsRemoteSensitiveSession reports true for. Nil ⇒
	// none (unchanged default), so existing tests see no attach sessions.
	attachHandles map[string]bool
	// snapshot is the live-session list Snapshot() returns. Nil ⇒ empty
	// (unchanged default), so existing tests see no live runs.
	snapshot []LaunchInfo
}

func (m *fakeLaunchManager) Create(spec LaunchSpec) (string, error) {
	if m.createErr != nil {
		return "", m.createErr
	}
	m.lastSpec = spec
	return "HANDLE-abc", nil
}

func (m *fakeLaunchManager) CreateFresh(spec FreshLaunchSpec) (string, error) {
	if m.freshErr != nil {
		return "", m.freshErr
	}
	m.lastFreshSpec = spec
	return "FRESH-abc", nil
}

func (m *fakeLaunchManager) CreateResume(spec ResumeLaunchSpec) (string, string, error) {
	if m.resumeErr != nil {
		return "", "", m.resumeErr
	}
	m.lastResumeSpec = spec
	return "RESUME-abc", "RUN-resume", nil
}

func (m *fakeLaunchManager) CreateSetup(spec SetupSpec) (string, error) {
	if m.setupErr != nil {
		return "", m.setupErr
	}
	m.lastSetupSpec = spec
	return "SETUP-abc", nil
}

func (m *fakeLaunchManager) Subscribe(handle string) (LaunchSubscription, error) {
	if m.subscribeErr != nil {
		return nil, m.subscribeErr
	}
	if m.sub == nil {
		m.sub = newFakeSubscription()
	}
	return m.sub, nil
}

func (m *fakeLaunchManager) SubscribeRemote(handle string) (LaunchSubscription, error) {
	return m.Subscribe(handle)
}

func (m *fakeLaunchManager) IsSetupSession(handle string) bool { return m.setupHandles[handle] }
func (m *fakeLaunchManager) IsRemoteSensitiveSession(handle string) bool {
	return m.attachHandles[handle]
}

func (m *fakeLaunchManager) Unsubscribe(LaunchSubscription) {}

func (m *fakeLaunchManager) AcquireWriterLocal(string) (LaunchWriter, error) {
	return newFakeWriter(), nil
}

func (m *fakeLaunchManager) AcquireWriterRemote(RemoteWriterRequest) (LaunchWriter, error) {
	return nil, ErrLaunchExecuteUnavailable
}

func (m *fakeLaunchManager) Close(string)                                   {}
func (m *fakeLaunchManager) SessionForRun(string) (string, bool)            { return "", false }
func (m *fakeLaunchManager) Snapshot() []LaunchInfo                         { return m.snapshot }
func (m *fakeLaunchManager) RevokeAllRemoteWriters(string) int              { return 0 }
func (m *fakeLaunchManager) RevokeRemoteWriterByHolder(string, string) bool { return false }

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

// --- POST /api/terminal/launch (F1 fresh-agent launch) ---

func postTerminalLaunch(t *testing.T, h http.Handler, tool, projectRoot string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(terminalLaunchRequest{Tool: tool, ProjectRoot: projectRoot})
	req := httptest.NewRequest(http.MethodPost, "/api/terminal/launch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTerminalLaunchDisabledWhenNilManager(t *testing.T) {
	s := newLaunchTestServer(t, nil)
	rec := postTerminalLaunch(t, s.Handler(), "claude-code", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil manager: status = %d, want 503", rec.Code)
	}
}

func TestTerminalLaunchRejectsUnlaunchableTool(t *testing.T) {
	s := newLaunchTestServer(t, &fakeLaunchManager{})
	rec := postTerminalLaunch(t, s.Handler(), "cline", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unlaunchable tool: status = %d, want 400", rec.Code)
	}
}

func TestTerminalLaunchMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"fresh disabled → 403", ErrLaunchFreshDisabled, http.StatusForbidden},
		{"tool not allowed → 403", ErrLaunchToolNotAllowed, http.StatusForbidden},
		{"project root denied → 400", ErrLaunchProjectRootDenied, http.StatusBadRequest},
		{"too many → 429", ErrLaunchTooMany, http.StatusTooManyRequests},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newLaunchTestServer(t, &fakeLaunchManager{freshErr: tc.err})
			rec := postTerminalLaunch(t, s.Handler(), "claude-code", "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTerminalLaunchSuccess(t *testing.T) {
	lm := &fakeLaunchManager{}
	s := newLaunchTestServer(t, lm)
	rec := postTerminalLaunch(t, s.Handler(), "codex", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var out terminalLaunchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" || out.Tool != "codex" || out.Subcommand != "codex" {
		t.Fatalf("response = %+v", out)
	}
	// The server resolved the subcommand from the registry, not the client.
	if lm.lastFreshSpec.Tool != "codex" || lm.lastFreshSpec.Subcommand != "codex" {
		t.Fatalf("fresh spec = %+v", lm.lastFreshSpec)
	}
}

func TestTerminalSessionsList(t *testing.T) {
	s := newLaunchTestServer(t, &fakeLaunchManager{})
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/sessions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sessions") {
		t.Errorf("body missing sessions key: %s", rec.Body.String())
	}
	// The launch surface always advertises the allow-list (empty here — no
	// ConfigPath) so the New-terminal dialog can mark permitted roots.
	if !strings.Contains(rec.Body.String(), "allowed_project_roots") {
		t.Errorf("body missing allowed_project_roots key: %s", rec.Body.String())
	}
}

// getTerminalSessionsRoots GETs /api/terminal/sessions and returns the
// allowed_project_roots field the New-terminal dialog reads.
func getTerminalSessionsRoots(t *testing.T, h http.Handler) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/sessions", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/terminal/sessions = %d", rec.Code)
	}
	var out struct {
		AllowedProjectRoots []string `json:"allowed_project_roots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.AllowedProjectRoots
}

// TestTerminalSessionsIncludesAllowedProjectRoots asserts the sessions endpoint
// surfaces the operator's canonicalized [terminal.launch].allowed_project_roots
// — empty (deny-all) before any allow-list, then the canonical directory after
// one is configured — so the dialog's permitted marking matches the spawn-time
// ValidateProjectRoot check without re-canonicalizing client-side.
func TestTerminalSessionsIncludesAllowedProjectRoots(t *testing.T) {
	h := newManageServerWithLaunch(t, &fakeLaunchManager{})

	if roots := getTerminalSessionsRoots(t, h); len(roots) != 0 {
		t.Fatalf("expected empty allowed_project_roots initially, got %v", roots)
	}

	dir := t.TempDir()
	tool := launchableTools()[0]
	body := `{"allow_fresh_agent":true,"allowed_tools":["` + tool +
		`"],"allowed_project_roots":["` + strings.ReplaceAll(dir, `\`, `\\`) + `"]}`
	if code, out := putPolicy(t, h, body); code != http.StatusOK {
		t.Fatalf("PUT policy = %d (%v)", code, out)
	}

	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	roots := getTerminalSessionsRoots(t, h)
	if len(roots) != 1 || roots[0] != want {
		t.Fatalf("allowed_project_roots = %v, want [%s]", roots, want)
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
	lm := &fakeLaunchManager{sub: newFakeSubscription()}
	t.Cleanup(func() { close(lm.sub.release) })
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
	// Subscribe fails → the upgrade is accepted (same-origin) then closed with
	// a policy-violation status; Dial itself succeeds at the handshake.
	lm := &fakeLaunchManager{subscribeErr: ErrLaunchAlreadyAttached}
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
