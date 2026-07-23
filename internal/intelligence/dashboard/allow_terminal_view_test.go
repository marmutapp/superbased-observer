package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
)

// --- Settings section: [terminal.attach] save/preserve ---

// TestHandleConfigSection_SaveTerminalAttach pins the Phase-4 Settings "Session
// attach" control: PUT /api/config/section/terminal writes ONLY
// [terminal.attach].enabled + route_proxy, persists to the TOML, round-trips
// through config.Load, and PRESERVES every other [terminal] sub-struct
// (terminal.enabled, terminal.launch.*, terminal.status.*) — the same selective
// copy the process section uses.
func TestHandleConfigSection_SaveTerminalAttach(t *testing.T) {
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`[terminal]
enabled = true

[terminal.launch]
allow_fresh_agent = true
allowed_tools = ["claude-code"]

[terminal.status]
enabled = true

[terminal.attach]
enabled = true
route_proxy = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
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
		httptest.NewRequest(http.MethodPut, "/api/config/section/terminal",
			strings.NewReader(`{"Enabled":false,"RouteProxy":false}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("terminal section save: status %d body=%s", rr.Code, rr.Body.String())
	}
	// The section binds at start, so restart_required must be true (honest for
	// the socket-serving field).
	var saveResp struct {
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &saveResp)
	if !saveResp.RestartRequired {
		t.Errorf("terminal section save must report restart_required=true (socket binds at start)")
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	tc := reloaded.Terminal
	// Editable fields persisted.
	if tc.Attach.Enabled {
		t.Errorf("Attach.Enabled not persisted (want false), got true")
	}
	if tc.Attach.RouteProxy {
		t.Errorf("Attach.RouteProxy not persisted (want false), got true")
	}
	// Sibling [terminal] fields preserved — not clobbered by the narrow write.
	if !tc.Enabled {
		t.Errorf("terminal.enabled clobbered (want preserved true)")
	}
	if !tc.Launch.AllowFreshAgent {
		t.Errorf("terminal.launch.allow_fresh_agent clobbered (want preserved true)")
	}
	if len(tc.Launch.AllowedTools) != 1 || tc.Launch.AllowedTools[0] != "claude-code" {
		t.Errorf("terminal.launch.allowed_tools clobbered: %+v", tc.Launch.AllowedTools)
	}
	if !tc.Status.Enabled {
		t.Errorf("terminal.status.enabled clobbered (want preserved true)")
	}

	// "terminal" is advertised in the editable-sections list.
	grr := httptest.NewRecorder()
	server.Handler().ServeHTTP(grr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if !strings.Contains(grr.Body.String(), `"terminal"`) {
		t.Errorf("editable_sections must advertise \"terminal\": %s", grr.Body.String())
	}
}

// --- Manage verb: /api/remote/allow-terminal-view ---

// TestAllowTerminalViewManageVerb pins the Local manage verb that flips
// [remote].allow_terminal_view on an armed controller: confirm-token gated,
// precondition-checked, strict-decoded, hot-reloaded onto the live gate, and —
// on →false — closes every open remote-sensitive viewer + writes an audit row.
func TestAllowTerminalViewManageVerb(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	// Precondition: remote off ⇒ 400 (naming the off state), never a 500.
	if rec := postConfirm(t, h, "/api/remote/allow-terminal-view", `{"allow_terminal_view":true}`, ck, token); rec.Code != http.StatusBadRequest {
		t.Fatalf("view toggle with remote off = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Arm remote so the toggle has an enabled config to write against.
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net"}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}

	// Strict decode: a body with no boolean field ⇒ 400 (never a silent
	// default-to-false disable).
	if rec := postConfirm(t, h, "/api/remote/allow-terminal-view", `{}`, ck, token); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty-body view toggle = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	mc := s.opts.Remote.(*remoteController)

	// Enable the view opt-in.
	rec := postConfirm(t, h, "/api/remote/allow-terminal-view", `{"allow_terminal_view":true}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable view = %d: %s", rec.Code, rec.Body.String())
	}
	var onResp struct {
		OK                bool `json:"ok"`
		RestartRequired   bool `json:"restart_required"`
		AllowTerminalView bool `json:"allow_terminal_view"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &onResp)
	if !onResp.OK || onResp.RestartRequired || !onResp.AllowTerminalView {
		t.Fatalf("enable-view response unexpected: %+v", onResp)
	}
	// Persisted to config + hot-swapped onto the live controller.
	cfg, _ := loadConfigForDashboard(s.opts.ConfigPath)
	if !cfg.Remote.AllowTerminalView {
		t.Errorf("allow_terminal_view not persisted to config")
	}
	if !mc.AllowTerminalView() {
		t.Errorf("live controller AllowTerminalView() not hot-swapped on")
	}

	// A live remote-sensitive viewer is registered; disabling the opt-in must
	// close it NOW (the read-side revoke-kills-open-viewers invariant).
	closed := make(chan struct{})
	unregister := s.registerSensitiveViewer("dev-fp", func() { close(closed) })
	defer unregister()

	rec = postConfirm(t, h, "/api/remote/allow-terminal-view", `{"allow_terminal_view":false}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable view = %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-closed:
	default:
		t.Errorf("allow_terminal_view→false did NOT close the open remote-sensitive viewer")
	}
	cfg, _ = loadConfigForDashboard(s.opts.ConfigPath)
	if cfg.Remote.AllowTerminalView {
		t.Errorf("allow_terminal_view not persisted false")
	}
	if mc.AllowTerminalView() {
		t.Errorf("live controller AllowTerminalView() not hot-swapped off")
	}

	// A metadata-only audit row records the toggle.
	arr := httptest.NewRequest(http.MethodGet, "/api/remote/audit", nil)
	arr.Host = "127.0.0.1:8080"
	arec := httptest.NewRecorder()
	h.ServeHTTP(arec, arr)
	if !strings.Contains(arec.Body.String(), "allow-terminal-view") {
		t.Errorf("audit tail missing the allow-terminal-view manage rows: %s", arec.Body.String())
	}
}

// --- Remote VIEW gate: deny by default, allow read-only under the opt-in ---

// newViewController builds a bare RemoteController whose only relevant state is
// the allow_terminal_view gate — enough to drive the dashboard-boundary VIEW
// gate (which reads AllowTerminalView() via a type assertion, never Ready()).
func newViewController(allowView bool) RemoteController {
	return NewRemoteController(RemoteOptions{AllowTerminalView: allowView})
}

// TestAllowTerminalViewGatesAttachAndResumeSubscriptions names and pins the
// independent read gate for both remote-sensitive run kinds. The default false
// refuses before SubscribeRemote; true permits a read-only subscription. It
// does not grant writer control.
func TestAllowTerminalViewGatesAttachAndResumeSubscriptions(t *testing.T) {
	for _, kind := range []string{"attach", "resume"} {
		for _, allow := range []bool{false, true} {
			name := kind + "/allow_terminal_view=" + strconv.FormatBool(allow)
			t.Run(name, func(t *testing.T) {
				handle := strings.ToUpper(kind) + "-SENSITIVE"
				lm := newRecordingLaunchManager(nil)
				lm.attachHandles = map[string]bool{handle: true}
				t.Cleanup(func() { close(lm.sub.release) })
				database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = database.Close() })
				s, err := New(Options{DB: database, LaunchManager: lm, Remote: newViewController(allow)})
				if err != nil {
					t.Fatal(err)
				}
				ts := remoteExposedWSServer(t, s)
				t.Cleanup(ts.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c, _, dialErr := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/launch/"+handle, &websocket.DialOptions{
					HTTPHeader: http.Header{"Origin": {ts.URL}},
				})
				if c != nil {
					defer c.CloseNow()
				}
				if allow {
					if dialErr != nil {
						t.Fatalf("allow_terminal_view=true dial: %v", dialErr)
					}
					deadline := time.Now().Add(time.Second)
					for lm.subscribeRemoteCalls.Load() == 0 && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
					}
					if lm.subscribeRemoteCalls.Load() != 1 {
						t.Fatalf("SubscribeRemote calls = %d, want 1", lm.subscribeRemoteCalls.Load())
					}
				} else {
					if dialErr == nil {
						_, _, dialErr = c.Read(ctx)
					}
					if dialErr == nil {
						t.Fatal("allow_terminal_view=false remote-sensitive subscription stayed open")
					}
					if lm.subscribeRemoteCalls.Load() != 0 {
						t.Fatalf("gate-off reached SubscribeRemote %d times", lm.subscribeRemoteCalls.Load())
					}
				}
			})
		}
	}
}

// TestRemoteViewAttachDeniedWhenViewOff pins that a controller present but with
// allow_terminal_view=false keeps the deny-by-default: the remote snapshot hides
// the attach handle and the remote WS is refused before any output (identical to
// the no-controller case the existing suite pins).
func TestRemoteViewAttachDeniedWhenViewOff(t *testing.T) {
	lm := newRecordingLaunchManager(nil)
	lm.attachHandles = map[string]bool{"ATTACH-1": true}
	lm.snapshot = []LaunchInfo{
		{ID: "ATTACH-1", Kind: "attach", Tool: "claude-code", Subcommand: "claude"},
		{ID: "REG-1", Kind: "fresh", Subcommand: "claude"},
	}
	t.Cleanup(func() { close(lm.sub.release) })

	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, LaunchManager: lm, Remote: newViewController(false)})
	if err != nil {
		t.Fatal(err)
	}
	ts := remoteExposedWSServer(t, s)
	defer ts.Close()

	remoteBody := httpGetBody(t, ts.URL+"/api/launch/sessions")
	if strings.Contains(remoteBody, "ATTACH-1") {
		t.Errorf("view-off remote snapshot leaked the attach handle: %s", remoteBody)
	}

	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, derr := websocket.Dial(ctx, base+"/ws/launch/ATTACH-1", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if derr == nil {
		if _, _, rerr := c.Read(ctx); rerr == nil {
			t.Error("view-off remote WS streamed data — it must be refused")
		}
		_ = c.CloseNow()
	}
	if n := lm.subscribeRemoteCalls.Load(); n != 0 {
		t.Errorf("view-off remote WS must NOT reach SubscribeRemote, got %d", n)
	}
}

// TestRemoteViewAttachAllowedUnderOptIn pins the Phase-4 relaxation: with
// allow_terminal_view=true, a remote caller may SEE the attach row and READ-ONLY
// subscribe to it (SubscribeRemote is reached), but WRITER acquisition is still
// refused — the view opt-in never relaxes the execute-tier write conjunction.
func TestRemoteViewAttachAllowedUnderOptIn(t *testing.T) {
	lm := newRecordingLaunchManager(nil) // remoteWriter nil ⇒ AcquireWriterRemote denies
	lm.attachHandles = map[string]bool{"ATTACH-1": true}
	lm.snapshot = []LaunchInfo{
		{ID: "ATTACH-1", Kind: "attach", Tool: "claude-code", Subcommand: "claude"},
		{ID: "REG-1", Kind: "fresh", Subcommand: "claude"},
	}
	t.Cleanup(func() { close(lm.sub.release) })

	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, LaunchManager: lm, Remote: newViewController(true)})
	if err != nil {
		t.Fatal(err)
	}
	ts := remoteExposedWSServer(t, s)
	defer ts.Close()

	// (1) The remote snapshot now REVEALS the attach handle (view opted in).
	remoteBody := httpGetBody(t, ts.URL+"/api/launch/sessions")
	if !strings.Contains(remoteBody, "ATTACH-1") {
		t.Errorf("view-on remote snapshot must reveal the attach handle: %s", remoteBody)
	}

	// (2) A remote WS SUBSCRIBES (view) — reaching SubscribeRemote — and is NOT
	// closed at the boundary.
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, derr := websocket.Dial(ctx, base+"/ws/launch/ATTACH-1", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if derr != nil {
		t.Fatalf("view-on remote WS to an attach session must upgrade: %v", derr)
	}
	defer c.CloseNow()

	// (3) A writer-acquire frame is REFUSED — the view opt-in grants no write.
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"acquire-writer","cap":"bad","confirm":"bad"}`))
	// Give the read loop time to take the subscribe + process the acquire frame.
	time.Sleep(200 * time.Millisecond)

	if n := lm.subscribeRemoteCalls.Load(); n == 0 {
		t.Errorf("view-on remote WS must reach SubscribeRemote (read-only view admitted), got 0")
	}
	if n := lm.subscribeLocalCalls.Load(); n != 0 {
		t.Errorf("view-on remote WS must NOT take the owner-local Subscribe path, got %d", n)
	}
	if n := lm.localCalls.Load(); n != 0 {
		t.Errorf("view-on remote WS must NEVER take the owner-local writer, got %d", n)
	}
	if n := lm.remoteCalls.Load(); n == 0 {
		t.Errorf("expected the acquire-writer frame to attempt AcquireWriterRemote (then be denied)")
	}
	// remoteWriter is nil ⇒ the acquire was refused; no PTY write ever happened.
	if lm.localWriter.writes.Load() != 0 {
		t.Errorf("a viewer keystroke path reached the PTY writer under view-only opt-in")
	}
}
