package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
)

// enableCaptureTestServer writes a config.toml seeded with the given process
// enabled/backend, opens a DB, and returns an assembled Server + the config
// path. loopback Handler() (Local routes are ungated there).
func enableCaptureTestServer(t *testing.T, enabled bool, backend string) (*Server, string) {
	t.Helper()
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	cfg := config.Default()
	cfg.Observer.DBPath = filepath.Join(tdir, "state.db")
	cfg.Observer.Process.Enabled = enabled
	cfg.Observer.Process.Backend = backend
	if err := config.WriteToml(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// WriteToml leaves no .bak on the very first write to a nonexistent file, but
	// be defensive: remove any .bak so the no-write proof in the idempotent test
	// is unambiguous.
	_ = os.Remove(cfgPath + ".bak")
	database, err := db.Open(context.Background(), db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Default the injected platform-capability probe to a FULLY capable host
	// (poll + eBPF + WSL bridge all available) so backend preservation is
	// deterministic regardless of the CI host's real platform. Individual tests
	// override server.processCapabilityFn to model other GOOS shapes.
	server.processCapabilityFn = fixedCapability(processCapability{
		GOOS: "linux", PollOK: true, EBPFOK: true, BridgeOK: true,
	})
	return server, cfgPath
}

// fixedCapability returns a processCapabilityFn that ignores the ProcessConfig
// and always reports the given capability — the injected-IO seam that lets the
// enable-capture tests exercise every GOOS-shaped capability set without the
// real platform.
func fixedCapability(c processCapability) func(config.ProcessConfig) processCapability {
	return func(config.ProcessConfig) processCapability { return c }
}

func postEnableCapture(t *testing.T, server *Server) (int, enableCaptureResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/process/enable-capture", nil))
	var resp enableCaptureResponse
	if rr.Code == http.StatusOK {
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
		}
	}
	return rr.Code, resp
}

// TestProcessEnableCapture_SwitchesNonRunnableBackendToAuto proves the verb
// flips a backend the daemon's selector constructs NOTHING for (off / empty /
// the etw+endpointsecurity stubs) to "auto" and flags the switch. These are
// exactly the values processBackendRunnable reports false for.
func TestProcessEnableCapture_SwitchesNonRunnableBackendToAuto(t *testing.T) {
	for _, backend := range []string{"off", "", "etw", "endpointsecurity"} {
		t.Run("backend="+backend, func(t *testing.T) {
			server, cfgPath := enableCaptureTestServer(t, false, backend)
			code, resp := postEnableCapture(t, server)
			if code != http.StatusOK {
				t.Fatalf("status: %d", code)
			}
			if !resp.Enabled || resp.Backend != "auto" || !resp.SwitchedBackend {
				t.Fatalf("resp = %+v, want enabled+backend=auto+switched", resp)
			}
			if resp.PreviousBackend != backend {
				t.Errorf("previous_backend = %q, want %q", resp.PreviousBackend, backend)
			}
			if !resp.RestartRequired {
				t.Error("restart_required = false, want true (a write happened)")
			}
			got, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Observer.Process.Enabled || got.Observer.Process.Backend != "auto" {
				t.Errorf("persisted process = {enabled:%v backend:%q}, want {true auto}",
					got.Observer.Process.Enabled, got.Observer.Process.Backend)
			}
		})
	}
}

// TestProcessEnableCapture_PreservesRunnableBackend proves the verb turns
// capture on but PRESERVES a backend the selector actually builds — no switch,
// no previous_backend.
func TestProcessEnableCapture_PreservesRunnableBackend(t *testing.T) {
	for _, backend := range []string{"poll", "bridge", "both", "linux_ebpf", "auto"} {
		t.Run("backend="+backend, func(t *testing.T) {
			server, cfgPath := enableCaptureTestServer(t, false, backend)
			code, resp := postEnableCapture(t, server)
			if code != http.StatusOK {
				t.Fatalf("status: %d", code)
			}
			if !resp.Enabled || resp.Backend != backend || resp.SwitchedBackend {
				t.Fatalf("resp = %+v, want enabled+backend=%q+not-switched", resp, backend)
			}
			if resp.PreviousBackend != "" {
				t.Errorf("previous_backend = %q, want empty (preserved)", resp.PreviousBackend)
			}
			if !resp.RestartRequired {
				t.Error("restart_required = false, want true (a write happened)")
			}
			got, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Observer.Process.Enabled || got.Observer.Process.Backend != backend {
				t.Errorf("persisted process = {enabled:%v backend:%q}, want {true %q}",
					got.Observer.Process.Enabled, got.Observer.Process.Backend, backend)
			}
		})
	}
}

// TestProcessEnableCapture_IdempotentNoWrite proves that when capture is ALREADY
// enabled with a runnable backend, the verb responds success WITHOUT writing the
// file — proven by the config bytes being byte-identical and NO .bak (which
// config.WriteToml always creates on a real write) appearing afterwards.
func TestProcessEnableCapture_IdempotentNoWrite(t *testing.T) {
	server, cfgPath := enableCaptureTestServer(t, true, "poll")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	code, resp := postEnableCapture(t, server)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if !resp.Enabled || resp.Backend != "poll" || resp.SwitchedBackend {
		t.Fatalf("resp = %+v, want enabled+backend=poll+not-switched", resp)
	}
	if resp.RestartRequired {
		t.Error("restart_required = true on the idempotent no-op — want false (nothing was written)")
	}
	if resp.PreviousBackend != "" {
		t.Errorf("previous_backend = %q, want empty", resp.PreviousBackend)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("config.toml changed on the idempotent path — the verb must not write when already enabled + runnable")
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a .bak appeared (%v) — config.WriteToml creates .bak on every real write, so its presence proves an unexpected write", err)
	}
}

// TestProcessEnableCapture_SwitchesBackendNotRunnableHere proves that a backend
// the SELECTOR builds but that cannot capture on THIS host — the canonical case
// being an explicit "bridge" on a non-WSL host (poll works, bridge resolves no
// Windows executable) — is switched to "auto" with the prior value reported,
// exactly like the selector-builds-nothing values. Grounded in a capability
// where PollOK is true (host can capture something) but BridgeOK is false.
func TestProcessEnableCapture_SwitchesBackendNotRunnableHere(t *testing.T) {
	server, cfgPath := enableCaptureTestServer(t, false, "bridge")
	// Non-WSL host: poll captures, the bridge does not resolve here.
	server.processCapabilityFn = fixedCapability(processCapability{
		GOOS: "linux", PollOK: true, EBPFOK: false, BridgeOK: false,
	})
	code, resp := postEnableCapture(t, server)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if !resp.Enabled || resp.Backend != "auto" || !resp.SwitchedBackend {
		t.Fatalf("resp = %+v, want enabled+backend=auto+switched", resp)
	}
	if resp.PreviousBackend != "bridge" {
		t.Errorf("previous_backend = %q, want %q", resp.PreviousBackend, "bridge")
	}
	if !resp.RestartRequired {
		t.Error("restart_required = false, want true (a write happened)")
	}
	got, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Observer.Process.Enabled || got.Observer.Process.Backend != "auto" {
		t.Errorf("persisted process = {enabled:%v backend:%q}, want {true auto}",
			got.Observer.Process.Enabled, got.Observer.Process.Backend)
	}
}

// TestProcessEnableCapture_UnsupportedPlatformNoWrite proves that on a host with
// NO runnable capture backend at all (the darwin shape: poll stub, no eBPF
// off-linux, no WSL bridge) the verb refuses honestly — enabled=false,
// reason=unsupported_platform, a GOOS-named detail — and writes NOTHING (config
// bytes byte-identical, no .bak, which config.WriteToml always creates on a real
// write).
func TestProcessEnableCapture_UnsupportedPlatformNoWrite(t *testing.T) {
	server, cfgPath := enableCaptureTestServer(t, false, "auto")
	server.processCapabilityFn = fixedCapability(processCapability{
		GOOS: "darwin", PollOK: false, EBPFOK: false, BridgeOK: false,
	})
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	code, resp := postEnableCapture(t, server)
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if resp.Enabled {
		t.Errorf("enabled = true, want false on an unsupported platform")
	}
	if resp.Reason != "unsupported_platform" {
		t.Errorf("reason = %q, want %q", resp.Reason, "unsupported_platform")
	}
	if !strings.Contains(resp.Detail, "darwin") {
		t.Errorf("detail = %q, want it to name the GOOS (darwin)", resp.Detail)
	}
	if resp.SwitchedBackend || resp.RestartRequired {
		t.Errorf("resp = %+v, want no switch + no restart on the refusal path", resp)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("config.toml changed on the unsupported-platform path — the verb must not write")
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a .bak appeared (%v) — proves an unexpected write on the refusal path", err)
	}
}

// TestProcessEnableCapture_SwitchEmptyBackendReportsPrevious proves the Fix 3
// invariant that a switch ALWAYS carries previous_backend even when the prior
// value is empty (config never set a backend). previous_backend must serialize
// as "" (not be omitted) so the frontend can render it as "unset".
func TestProcessEnableCapture_SwitchEmptyBackendReportsPrevious(t *testing.T) {
	server, _ := enableCaptureTestServer(t, false, "")
	// Fully capable host so we reach the switch path (not the unsupported one).
	// Capture the RAW body of the first (switching) POST to confirm
	// previous_backend serializes even when the prior value is "".
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/process/enable-capture", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var resp enableCaptureResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.SwitchedBackend || resp.Backend != "auto" {
		t.Fatalf("resp = %+v, want switched+auto", resp)
	}
	if !strings.Contains(rr.Body.String(), `"previous_backend":""`) {
		t.Errorf("body %s missing an explicit empty previous_backend (Fix 3: dropped omitempty)", rr.Body.String())
	}
}

// TestProcessEnableCapture_NonPostIs405 pins the method guard.
func TestProcessEnableCapture_NonPostIs405(t *testing.T) {
	server, _ := enableCaptureTestServer(t, false, "poll")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/process/enable-capture", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status: %d, want 405", rr.Code)
	}
}

// TestProcessEnableCapture_RefusedOnRemoteListener proves the route's Local
// class: a paired VIEW principal (and any remote principal) is refused with 403
// on the remote-exposed listener, so a config-writing verb can never be driven
// remotely. (The generic TestLocalRoutesRefusedOnRemoteListener covers every
// Local route; this is the focused, self-documenting assertion for this one.)
func TestProcessEnableCapture_RefusedOnRemoteListener(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)
	cookie, csrf := pairSession(t, h, enc)

	req := httptest.NewRequest(http.MethodPost, "/api/process/enable-capture", strings.NewReader("{}"))
	req.Host = testRemoteHost
	req.Header.Set("Origin", "https://"+testRemoteHost)
	req.AddCookie(cookie)
	req.Header.Set(remoteCSRFHeader, csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote POST /api/process/enable-capture = %d, want 403 (Local route)", rec.Code)
	}
}
