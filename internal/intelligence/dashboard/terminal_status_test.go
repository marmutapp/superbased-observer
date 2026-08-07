package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// fakeStatusProvider is a static TerminalStatusProvider for handler tests.
type fakeStatusProvider struct {
	res   TerminalStatusResult
	found bool
	sub   *fakeStatusSub
}

func (f *fakeStatusProvider) StatusForHandle(handle string) (TerminalStatusResult, bool) {
	if !f.found {
		return TerminalStatusResult{}, false
	}
	r := f.res
	r.Handle = handle
	return r, true
}

func (f *fakeStatusProvider) Subscribe() TerminalStatusSubscription {
	if f.sub == nil {
		f.sub = &fakeStatusSub{ch: make(chan TerminalStatusResult, 1)}
	}
	return f.sub
}

type fakeStatusSub struct{ ch chan TerminalStatusResult }

func (s *fakeStatusSub) Updates() <-chan TerminalStatusResult { return s.ch }
func (s *fakeStatusSub) Close()                               {}

func newStatusTestServer(t *testing.T, p TerminalStatusProvider) *Server {
	t.Helper()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, TerminalStatus: p})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTerminalStatusGET(t *testing.T) {
	p := &fakeStatusProvider{found: true, res: TerminalStatusResult{
		Status: "waiting-for-input", Evidence: "prompt + silence", Confidence: "hint", AgeSeconds: 4,
	}}
	s := newStatusTestServer(t, p)
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/HANDLE-1/status", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var out TerminalStatusResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Handle != "HANDLE-1" || out.Status != "waiting-for-input" || out.Confidence != "hint" {
		t.Fatalf("result = %+v", out)
	}
}

func TestTerminalStatusUnknownHandle404(t *testing.T) {
	s := newStatusTestServer(t, &fakeStatusProvider{found: false})
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/nope/status", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTerminalStatusDisabled503(t *testing.T) {
	s := newStatusTestServer(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/x/status", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestTerminalStatusBadPath404(t *testing.T) {
	s := newStatusTestServer(t, &fakeStatusProvider{found: true})
	// Missing the /status suffix.
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/HANDLE-1/other", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- FIX A: SpecSetup handles are redacted from the terminal STATUS surface for
// remote-exposed callers (second adversarial review 2026-07-16). ---

// remoteWrap simulates the remote-exposed boundary: it stamps the SAME
// listener-provenance marker remoteAuthz sets, then serves through the real mux,
// so a test exercises the exact redaction path a paired remote device hits
// without standing up the full RemoteController auth stack.
func remoteWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withRemoteExposed(r.Context())))
	})
}

func newStatusLaunchTestServer(t *testing.T, p TerminalStatusProvider, lm LaunchManager) *Server {
	t.Helper()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, TerminalStatus: p, LaunchManager: lm})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTerminalStatusPointRedactsSetupFromRemote proves the point status endpoint
// 404s a REMOTE-exposed caller asking about a privileged setup handle (its
// existence stays unconfirmable, mirroring handleLaunchAdmin's remote 404) while
// a remote caller still reads a normal handle and the LOCAL owner reads the setup
// handle (their in-dashboard xterm needs it).
func TestTerminalStatusPointRedactsSetupFromRemote(t *testing.T) {
	const setupHandle = "SETUP-xyz"
	const normalHandle = "AGENT-abc"
	p := &fakeStatusProvider{found: true, res: TerminalStatusResult{Status: "working"}}
	lm := &fakeLaunchManager{setupHandles: map[string]bool{setupHandle: true}}
	s := newStatusLaunchTestServer(t, p, lm)

	get := func(handle string, remote bool) int {
		req := httptest.NewRequest(http.MethodGet, "/api/terminal/"+handle+"/status", nil)
		if remote {
			req = req.WithContext(withRemoteExposed(req.Context()))
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get(setupHandle, true); code != http.StatusNotFound {
		t.Fatalf("remote setup-handle status = %d, want 404 (redacted)", code)
	}
	if code := get(normalHandle, true); code != http.StatusOK {
		t.Fatalf("remote normal-handle status = %d, want 200", code)
	}
	if code := get(setupHandle, false); code != http.StatusOK {
		t.Fatalf("local owner setup-handle status = %d, want 200 (owner still sees it)", code)
	}
}

// statusStreamProvider is a controllable TerminalStatusProvider whose Subscribe
// returns a subscription fed by a test-controlled channel.
type statusStreamProvider struct{ ch chan TerminalStatusResult }

func (p *statusStreamProvider) StatusForHandle(string) (TerminalStatusResult, bool) {
	return TerminalStatusResult{}, false
}

func (p *statusStreamProvider) Subscribe() TerminalStatusSubscription {
	return &statusStreamSub{ch: p.ch}
}

type statusStreamSub struct{ ch chan TerminalStatusResult }

func (s *statusStreamSub) Updates() <-chan TerminalStatusResult { return s.ch }
func (s *statusStreamSub) Close()                               {}

// TestTerminalStatusStreamOmitsSetupForRemote proves the WS status stream drops
// a privileged setup handle's updates for a remote-exposed caller: a setup update
// pushed BEFORE a normal update never reaches the client — the first frame it
// receives is the normal handle, so the setup handle (and its activity timing)
// is omitted entirely.
func TestTerminalStatusStreamOmitsSetupForRemote(t *testing.T) {
	const setupHandle = "SETUP-xyz"
	const normalHandle = "AGENT-abc"
	ch := make(chan TerminalStatusResult, 8)
	lm := &fakeLaunchManager{setupHandles: map[string]bool{setupHandle: true}}
	s := newStatusLaunchTestServer(t, &statusStreamProvider{ch: ch}, lm)

	ts := httptest.NewServer(remoteWrap(s.Handler()))
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/terminal/status", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Push a setup update (must be dropped) then a normal update (must arrive).
	ch <- TerminalStatusResult{Handle: setupHandle, Status: "working"}
	ch <- TerminalStatusResult{Handle: normalHandle, Status: "idle"}

	var got TerminalStatusResult
	if err := wsjson.Read(ctx, c, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Handle != normalHandle {
		t.Fatalf("remote stream delivered handle %q, want %q (the setup handle must be omitted)", got.Handle, normalHandle)
	}
}

// TestTerminalStatusStreamLocalSeesSetup proves the owner-local loopback stream
// (no remote-exposed marker) still delivers setup-handle updates — the redaction
// is remote-only, so the in-dashboard xterm keeps working.
func TestTerminalStatusStreamLocalSeesSetup(t *testing.T) {
	const setupHandle = "SETUP-xyz"
	ch := make(chan TerminalStatusResult, 8)
	lm := &fakeLaunchManager{setupHandles: map[string]bool{setupHandle: true}}
	s := newStatusLaunchTestServer(t, &statusStreamProvider{ch: ch}, lm)

	ts := httptest.NewServer(s.Handler()) // no remoteWrap → owner-local path
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/ws/terminal/status", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	ch <- TerminalStatusResult{Handle: setupHandle, Status: "working"}

	var got TerminalStatusResult
	if err := wsjson.Read(ctx, c, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Handle != setupHandle {
		t.Fatalf("local stream delivered handle %q, want %q (owner must see setup handles)", got.Handle, setupHandle)
	}
}
