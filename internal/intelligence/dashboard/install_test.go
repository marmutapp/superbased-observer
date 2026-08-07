package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// install_test.go covers the tool-binary-resolution dashboard surface
// (tool-binary-resolution arc, Phase 5): the pre-launch verdict endpoint
// (GET /api/terminal/launch/preflight) and the guided install endpoint
// (POST /api/terminal/install). Both are httptest black-box tests over the real
// mux, mirroring launch_test.go / remote_tailscale_test.go.

// newPreflightServer builds a launch-test server wired with a ToolPreflight
// seam (nil ⇒ the endpoint's 501 disabled state).
func newPreflightServer(t *testing.T, seam func(string) (ToolPreflight, bool)) *Server {
	t.Helper()
	tdir := t.TempDir()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s, err := New(Options{DB: database, ToolPreflight: seam})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func getPreflight(t *testing.T, h http.Handler, tool string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/terminal/launch/preflight"
	if tool != "" {
		url += "?tool=" + tool
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTerminalPreflightNilSeam pins the honest disabled state: no ToolPreflight
// seam ⇒ 501 (not a fabricated verdict).
func TestTerminalPreflightNilSeam(t *testing.T) {
	s := newPreflightServer(t, nil)
	rec := getPreflight(t, s.Handler(), "claude-code")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil seam = %d, want 501: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalPreflightUnknownTool pins the 400 for a seam that reports the tool
// is not launchable (ok=false), mirroring handleTerminalLaunch's validation.
func TestTerminalPreflightUnknownTool(t *testing.T) {
	seam := func(string) (ToolPreflight, bool) { return ToolPreflight{}, false }
	s := newPreflightServer(t, seam)
	rec := getPreflight(t, s.Handler(), "not-a-tool")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown tool = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalPreflightMissingTool pins the 400 for an absent tool parameter.
func TestTerminalPreflightMissingTool(t *testing.T) {
	seam := func(string) (ToolPreflight, bool) { return ToolPreflight{}, true }
	s := newPreflightServer(t, seam)
	rec := getPreflight(t, s.Handler(), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing tool = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalPreflightVerdictPassthrough pins that the seam's verdict is
// serialized verbatim onto the wire (verdict/bin/notes/install_command/
// can_install), so the SPA renders the honest resolver result.
func TestTerminalPreflightVerdictPassthrough(t *testing.T) {
	want := ToolPreflight{
		Tool:           "opencode",
		Verdict:        "foreign_only",
		Bin:            "",
		Notes:          []string{"a Windows interop shim is earlier on PATH"},
		InstallCommand: "npm install -g opencode-ai@latest",
		CanInstall:     true,
	}
	seam := func(tool string) (ToolPreflight, bool) {
		if tool != "opencode" {
			return ToolPreflight{}, false
		}
		return want, true
	}
	s := newPreflightServer(t, seam)
	rec := getPreflight(t, s.Handler(), "opencode")
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got ToolPreflight
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verdict not passed through: got %+v want %+v", got, want)
	}
}

// TestTerminalPreflightMethodNotAllowed pins GET-only.
func TestTerminalPreflightMethodNotAllowed(t *testing.T) {
	seam := func(string) (ToolPreflight, bool) { return ToolPreflight{}, true }
	s := newPreflightServer(t, seam)
	req := httptest.NewRequest(http.MethodPost, "/api/terminal/launch/preflight?tool=codex", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST preflight = %d, want 405", rec.Code)
	}
}

// --- install endpoint ---

// newInstallServer builds a server with a Remote controller (so getConfirm's
// double-submit token works) plus the LaunchManager + guided-install seams.
func newInstallServer(t *testing.T, lm LaunchManager, allowInstall func() bool, hint func(string) ([]string, string, bool)) http.Handler {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	database, err := openTestDB(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[observer]\ndb_path = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rc, _ := newReadyRemoteController(t)
	s, err := New(Options{
		DB:               database,
		ConfigPath:       cfgPath,
		Remote:           rc,
		LaunchManager:    lm,
		AllowToolInstall: allowInstall,
		ToolInstallHint:  hint,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// postInstall issues a confirm-gated POST /api/terminal/install for tool.
func postInstall(t *testing.T, h http.Handler, tool string) *httptest.ResponseRecorder {
	t.Helper()
	ck, ctok := getConfirm(t, h)
	body, _ := json.Marshal(map[string]string{"tool": tool})
	req := httptest.NewRequest(http.MethodPost, "/api/terminal/install", bytes.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, ctok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var codexArgv = []string{"npm", "install", "-g", "@openai/codex"}

func codexHint(tool string) ([]string, string, bool) {
	if tool == "codex" {
		return codexArgv, "npm install -g @openai/codex", true
	}
	return nil, "", false
}

// TestTerminalInstallHappyPath pins the guided-install spawn: the fake manager
// captures the seam-provided registry argv VERBATIM (reflect.DeepEqual — proving
// the request contributed no argv) and the "install:<tool>" setup label.
func TestTerminalInstallHappyPath(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := newInstallServer(t, lm, func() bool { return true }, codexHint)
	rec := postInstall(t, h, "codex")
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"handle"`) || !strings.Contains(rec.Body.String(), `"command"`) {
		t.Errorf("response must carry handle + command: %s", rec.Body.String())
	}
	if !reflect.DeepEqual(lm.lastSetupSpec.Argv, codexArgv) {
		t.Errorf("spawned argv = %v, want the registry constant %v", lm.lastSetupSpec.Argv, codexArgv)
	}
	if lm.lastSetupSpec.Label != "install:codex" {
		t.Errorf("setup label = %q, want %q", lm.lastSetupSpec.Label, "install:codex")
	}
}

// TestTerminalInstallDisabledGate pins the kill-switch: allow_install off ⇒ 403
// whose message names the exact config key the operator must flip.
func TestTerminalInstallDisabledGate(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := newInstallServer(t, lm, func() bool { return false }, codexHint)
	rec := postInstall(t, h, "codex")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled install = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[terminal.launch].allow_install") {
		t.Errorf("403 must name the config key: %s", rec.Body.String())
	}
	if lm.lastSetupSpec.Argv != nil {
		t.Errorf("no PTY must spawn when disabled: %v", lm.lastSetupSpec.Argv)
	}
}

// TestTerminalInstallNilAllowSeam pins that a nil AllowToolInstall seam is
// treated as DISABLED (403), not open.
func TestTerminalInstallNilAllowSeam(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := newInstallServer(t, lm, nil, codexHint)
	rec := postInstall(t, h, "codex")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("nil allow seam = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalInstallNoHint pins the 400 for a tool with no grounded install
// command (the honesty floor).
func TestTerminalInstallNoHint(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := newInstallServer(t, lm, func() bool { return true }, codexHint)
	rec := postInstall(t, h, "pi") // codexHint returns ok=false for anything but codex
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-hint install = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no grounded install command") {
		t.Errorf("400 must name the missing-hint reason: %s", rec.Body.String())
	}
}

// TestTerminalInstallInFlight pins the 409 single-flight passthrough: the setup
// spawn sentinel (a privileged PTY of this kind already starting) maps to 409
// via writeSetupSpawnErr, exactly like the Tailscale setup handlers.
func TestTerminalInstallInFlight(t *testing.T) {
	lm := &fakeLaunchManager{setupErr: ErrLaunchSetupInFlight}
	h := newInstallServer(t, lm, func() bool { return true }, codexHint)
	rec := postInstall(t, h, "codex")
	if rec.Code != http.StatusConflict {
		t.Fatalf("in-flight install = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalInstallServiceUnavailable pins the 503 when the PTY launcher is
// absent (nil LaunchManager).
func TestTerminalInstallServiceUnavailable(t *testing.T) {
	h := newInstallServer(t, nil, func() bool { return true }, codexHint)
	rec := postInstall(t, h, "codex")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil manager install = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestTerminalInstallConfirmAndMethod pins the CSRF/method hardening: a missing
// confirm token must not succeed, and a non-POST method is 405.
func TestTerminalInstallConfirmAndMethod(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := newInstallServer(t, lm, func() bool { return true }, codexHint)

	// Missing confirm token → not 200 (requireConfirmToken rejects).
	body, _ := json.Marshal(map[string]string{"tool": "codex"})
	req := httptest.NewRequest(http.MethodPost, "/api/terminal/install", bytes.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("missing confirm token must not succeed (got 200): %s", rec.Body.String())
	}

	// Wrong method → 405.
	req2 := httptest.NewRequest(http.MethodGet, "/api/terminal/install", nil)
	req2.Host = "127.0.0.1:8080"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET install = %d, want 405", rec2.Code)
	}
}
