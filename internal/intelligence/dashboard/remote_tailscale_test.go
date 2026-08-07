package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/tailnet"
)

// TestTailscaleStatusDegradesWhenAbsent pins the P1 honesty rule: the handler
// never errors the panel when tailscale is absent — it returns present:false as
// a first-class state (plan §D risk note). An empty PATH guarantees absence.
func TestTailscaleStatusDegradesWhenAbsent(t *testing.T) {
	t.Setenv("PATH", "")
	_, h := newManageServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/remote/tailscale/status", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET tailscale/status = %d (must never error the panel)", rec.Code)
	}
	var resp struct {
		Present         bool   `json:"present"`
		LoggedIn        bool   `json:"logged_in"`
		Host            string `json:"host"`
		InstallURL      string `json:"install_url"`
		DaemonRunsServe bool   `json:"daemon_runs_serve"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Present {
		t.Error("present must be false with an empty PATH")
	}
	if resp.LoggedIn || resp.Host != "" {
		t.Error("absent tailscale must not report logged_in/host")
	}
	if resp.InstallURL == "" {
		t.Error("install_url must be offered when tailscale is absent")
	}
	if resp.DaemonRunsServe {
		t.Error("daemon must never claim to run tailscale serve (honesty rule §D)")
	}
}

// TestTailscaleStatusMethodNotAllowed pins GET-only.
func TestTailscaleStatusMethodNotAllowed(t *testing.T) {
	_, h := newManageServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/status", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}

// TestRemoteTailscaleServe covers the "do it for the operator" path: arm to mint
// a backend port, then POST serve with a stubbed runner (deterministic — no real
// tailscale exec). Pins: 400 before arming, and the runner result passthrough.
func TestRemoteTailscaleServe(t *testing.T) {
	prevDetect := enableHostDetector
	enableHostDetector = func(context.Context) string { return "box.ts.net" }
	defer func() { enableHostDetector = prevDetect }()
	prevRun := tailscaleServeRunner
	defer func() { tailscaleServeRunner = prevRun }()

	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	// Before arming: no backend addr → 400 precondition, not a 500.
	tailscaleServeRunner = func(context.Context, string) tailnet.ServeResult {
		t.Fatal("runner must not be called before arming")
		return tailnet.ServeResult{}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/serve", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("serve before arm = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Arm (auto-detected host), then serve — runner result flows back.
	armReq := httptest.NewRequest(http.MethodPost, "/api/remote/enable", strings.NewReader(`{}`))
	armReq.Host = "127.0.0.1:8080"
	armReq.Header.Set("Content-Type", "application/json")
	armReq.AddCookie(ck)
	armReq.Header.Set(remoteConfirmHeader, token)
	armRec := httptest.NewRecorder()
	h.ServeHTTP(armRec, armReq)
	if armRec.Code != http.StatusOK {
		t.Fatalf("arm = %d: %s", armRec.Code, armRec.Body.String())
	}

	tailscaleServeRunner = func(context.Context, string) tailnet.ServeResult {
		return tailnet.ServeResult{OK: true, Output: "ok"}
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/serve", nil)
	req2.Host = "127.0.0.1:8080"
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(ck)
	req2.Header.Set(remoteConfirmHeader, token)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("serve = %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"ok":true`) {
		t.Errorf("serve response should pass runner OK through: %s", rec2.Body.String())
	}
}

// newManageServerWithLaunch is newManageServer plus an injected LaunchManager,
// for the operator-grant PTY-spawn path.
func newManageServerWithLaunch(t *testing.T, lm LaunchManager) http.Handler {
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
	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc, LaunchManager: lm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

// TestRemoteTailscaleOperatorGrant covers the guided privilege fix: the route
// spawns the operator grant in a local-only PTY. It is confirm-gated, 503s when
// the terminal is unavailable, and returns a handle on the happy path (or 400
// when the daemon is root — the grant is moot). Deterministic across CI (root)
// and dev (non-root) by branching on the real daemon identity.
func TestRemoteTailscaleOperatorGrant(t *testing.T) {
	_, isRoot, err := tailnet.CurrentDaemonUser()
	if err != nil {
		t.Skipf("CurrentDaemonUser unavailable on this host: %v", err)
	}

	t.Run("503 when terminal unavailable (nil LaunchManager)", func(t *testing.T) {
		_, h := newManageServer(t) // no LaunchManager
		ck, token := getConfirm(t, h)
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/operator-grant", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil LaunchManager = %d, want 503: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("confirm-gated", func(t *testing.T) {
		h := newManageServerWithLaunch(t, &fakeLaunchManager{})
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/operator-grant", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("missing confirm token must not succeed (got 200)")
		}
	})

	t.Run("spawns local-only setup PTY (or 400 as root)", func(t *testing.T) {
		lm := &fakeLaunchManager{}
		h := newManageServerWithLaunch(t, lm)
		ck, token := getConfirm(t, h)
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/operator-grant", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if isRoot {
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("root daemon = %d, want 400 (grant moot): %s", rec.Code, rec.Body.String())
			}
			return
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("operator-grant = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"handle"`) {
			t.Errorf("response must carry the PTY handle: %s", rec.Body.String())
		}
		// The spawned argv is the server-derived operator grant, never client input.
		if len(lm.lastSetupSpec.Argv) < 4 || lm.lastSetupSpec.Argv[0] != "sudo" ||
			lm.lastSetupSpec.Argv[1] != "tailscale" || !strings.HasPrefix(lm.lastSetupSpec.Argv[3], "--operator=") {
			t.Errorf("setup argv is not the server-derived operator grant: %v", lm.lastSetupSpec.Argv)
		}
	})
}

// stubTailscaleOnPath prepends a temp dir carrying an executable `tailscale`
// shim to PATH so tailnet.Detect reports Present=true deterministically in the
// dashboard package (mirrors internal/tailnet's withStubTailscale). POSIX-only.
func stubTailscaleOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script tailscale shim is POSIX-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRemoteTailscaleLogin covers the guided login: the route spawns
// `tailscale up` in a local-only PTY. It is confirm-gated, 503s when the
// terminal is unavailable, and returns a handle with the server-derived argv
// (sudo-prefixed unless the daemon is root). Login is valid for a root daemon
// too, so there is no root refusal (unlike the operator grant).
func TestRemoteTailscaleLogin(t *testing.T) {
	_, isRoot, err := tailnet.CurrentDaemonUser()
	if err != nil {
		t.Skipf("CurrentDaemonUser unavailable on this host: %v", err)
	}

	t.Run("503 when terminal unavailable (nil LaunchManager)", func(t *testing.T) {
		_, h := newManageServer(t) // no LaunchManager
		ck, confirmTok := getConfirm(t, h)
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/login", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, confirmTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil LaunchManager = %d, want 503: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("confirm-gated", func(t *testing.T) {
		h := newManageServerWithLaunch(t, &fakeLaunchManager{})
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/login", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("missing confirm token must not succeed (got 200)")
		}
	})

	t.Run("spawns local-only setup PTY with server-derived argv", func(t *testing.T) {
		lm := &fakeLaunchManager{}
		h := newManageServerWithLaunch(t, lm)
		ck, confirmTok := getConfirm(t, h)
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/login", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, confirmTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"handle"`) {
			t.Errorf("response must carry the PTY handle: %s", rec.Body.String())
		}
		want := tailnet.LoginArgv(isRoot)
		got := lm.lastSetupSpec.Argv
		if len(got) != len(want) {
			t.Fatalf("login argv = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("login argv = %v, want %v (server-derived, never client input)", got, want)
			}
		}
	})
}

// TestRemoteTailscaleInstall covers the guided Linux install: confirm-gated,
// refused when tailscale is already present, and (on Linux, when absent) spawns
// the fixed install-script argv in a local-only PTY. Off-Linux the route always
// 400s. Deterministic across CI/dev by stubbing tailscale presence via PATH.
func TestRemoteTailscaleInstall(t *testing.T) {
	t.Run("confirm-gated", func(t *testing.T) {
		h := newManageServerWithLaunch(t, &fakeLaunchManager{})
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/install", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("missing confirm token must not succeed (got 200)")
		}
	})

	t.Run("refuses when tailscale already present (400)", func(t *testing.T) {
		stubTailscaleOnPath(t) // Detect().Present == true
		lm := &fakeLaunchManager{}
		h := newManageServerWithLaunch(t, lm)
		ck, confirmTok := getConfirm(t, h)
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/install", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, confirmTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// On Linux the present-check fires; off-Linux the GOOS-check fires first.
		// Either way the guided install must be refused with 400 and never spawn.
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("install with tailscale present = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if lm.lastSetupSpec.Argv != nil {
			t.Errorf("install must not spawn when refused: %v", lm.lastSetupSpec.Argv)
		}
	})

	t.Run("spawns fixed install argv when absent (Linux) / 400 off-Linux", func(t *testing.T) {
		t.Setenv("PATH", "") // Detect().Present == false
		lm := &fakeLaunchManager{}
		h := newManageServerWithLaunch(t, lm)
		ck, confirmTok := getConfirm(t, h)
		req := httptest.NewRequest(http.MethodPost, "/api/remote/tailscale/install", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, confirmTok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if runtime.GOOS != "linux" {
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("off-Linux install = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			return
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("install (absent, Linux) = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"handle"`) {
			t.Errorf("response must carry the PTY handle: %s", rec.Body.String())
		}
		want := tailnet.InstallArgv()
		got := lm.lastSetupSpec.Argv
		if len(got) != len(want) {
			t.Fatalf("install argv = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("install argv = %v, want %v (fixed closed enum, never client input)", got, want)
			}
		}
	})
}
