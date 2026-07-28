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
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/processbridge/setup"
)

// process_etw_review_test.go pins the outcomes of the 2026-07-27 independent
// review of the dashboard-driven ETW setup feature. Each test names the finding
// it closes and asserts the BEHAVIOUR, not the wording.

// etwReviewServer is etwRegisterServer with the MASTER switch
// ([observer.process].enabled) under the test's control — the switch review
// finding 7 is about.
func etwReviewServer(t *testing.T, lm LaunchManager, masterEnabled, etwEnabled bool, env func() setup.Env) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.Observer.DBPath = filepath.Join(dir, "observer.db")
	cfg.Observer.Process.Enabled = masterEnabled
	cfg.Observer.Process.ETW.Enabled = etwEnabled
	if err := config.WriteToml(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	rc, _ := newReadyRemoteController(t)
	s, err := New(Options{
		DB:            database,
		DBPath:        cfg.Observer.DBPath,
		ConfigPath:    cfgPath,
		Remote:        rc,
		LaunchManager: lm,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if env == nil {
		env = func() setup.Env { return setup.Env{} }
	}
	s.etwSetupEnvFn = env
	return s.Handler()
}

// getETWStatusRemote issues the same GET as getETWStatus but through the
// remote-exposed provenance marker the remote authz chain stamps — i.e. as a
// PAIRED REMOTE DEVICE, not the local owner.
func getETWStatusRemote(t *testing.T, server *Server) etwStatusResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/process/etw/status", nil)
	req = req.WithContext(withRemoteExposed(req.Context()))
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp etwStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	return resp
}

// TestETWStatusWithholdsLocalDetailFromRemote closes review finding 6: the
// status route is capability V, so a PAIRED REMOTE DEVICE can reach it, and its
// Command + Notes disclose the observer.exe path, the shared-token file path and
// the Windows username to that caller.
//
// The health block and the state ladder stay available — a remote user reading
// "capturer connected / 0 dropped" is legitimate and useful.
func TestETWStatusWithholdsLocalDetailFromRemote(t *testing.T) {
	server, _ := etwStatusServer(t, func(c *config.Config) {
		c.Observer.Process.Enabled = true
		c.Observer.Process.ETW.Enabled = true
	})
	server.etwSetupEnvFn = func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	}

	local := getETWStatus(t, server)
	if local.Command == "" {
		t.Fatalf("precondition: the LOCAL response must carry the copyable command, got %+v", local)
	}
	if len(local.Notes) == 0 {
		t.Fatal("precondition: the LOCAL response must carry the planner notes")
	}
	if !strings.Contains(strings.Join(local.Notes, "\n"), "operator") {
		t.Fatalf("precondition: the notes name the Windows user; got %q", local.Notes)
	}

	remote := getETWStatusRemote(t, server)
	if remote.Command != "" {
		t.Errorf("remote caller received the full elevated command (paths + token file): %q", remote.Command)
	}
	if len(remote.Notes) != 0 {
		t.Errorf("remote caller received the planner notes (Windows username + token path): %q", remote.Notes)
	}
	if remote.CommandCmdShellOnly {
		t.Error("command_cmd_shell_only must be false when no command is disclosed")
	}
	if !remote.LocalDetailWithheld {
		t.Error("the response must SAY that detail was withheld, not silently omit it")
	}
	// The useful, non-disclosing half must survive.
	if !remote.PlanDetectable || remote.State != etwStateManual {
		t.Errorf("remote state = %q detectable=%v, want the ladder intact", remote.State, remote.PlanDetectable)
	}
	if remote.Probe != etwProbeAbsent {
		t.Errorf("remote probe = %q, want the probe tri-state intact", remote.Probe)
	}
	if !remote.SchtasksPresent {
		t.Error("schtasks_present must survive — it is not a disclosure")
	}
}

// TestETWStatusRemoteWithholdsPathBearingReasons closes the other half of
// finding 6: the BLOCKED reason and the plan-undetectable reason both quote
// filesystem paths verbatim (the §W4.6 #6 "name the path it tried" wording).
func TestETWStatusRemoteWithholdsPathBearingReasons(t *testing.T) {
	t.Run("blocked reason quotes the configured path", func(t *testing.T) {
		server, _ := etwStatusServer(t, func(c *config.Config) {
			c.Observer.Process.Enabled = true
			c.Observer.Process.ETW.Enabled = true
			c.Observer.Process.WindowsBinaryPath = `/mnt/c/nope/observer.exe`
		})
		server.etwSetupEnvFn = func() setup.Env {
			return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, "")
		}
		local := getETWStatus(t, server)
		if !strings.Contains(local.Reason, "/mnt/c/nope/observer.exe") {
			t.Fatalf("precondition: the local blocked reason names the path tried; got %q", local.Reason)
		}
		remote := getETWStatusRemote(t, server)
		if strings.Contains(remote.Reason, "/mnt/c/nope/observer.exe") {
			t.Errorf("remote caller learned a filesystem path from the blocked reason: %q", remote.Reason)
		}
		if remote.State != etwStateBlocked {
			t.Errorf("remote state = %q, want blocked (the STATE is not a disclosure)", remote.State)
		}
	})

	t.Run("plan-undetectable reason quotes the config path", func(t *testing.T) {
		server, _ := etwStatusServer(t, nil)
		// A config that exists but cannot be PARSED — the branch whose message
		// quotes the path. (A missing file loads defaults and is not an error.)
		bad := filepath.Join(t.TempDir(), "someone-config.toml")
		if err := os.WriteFile(bad, []byte("this is not = = toml\n"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		server.opts.ConfigPath = bad
		local := getETWStatus(t, server)
		if !strings.Contains(local.PlanUndetectableReason, bad) {
			t.Fatalf("precondition: the local reason names the config path; got %q", local.PlanUndetectableReason)
		}
		remote := getETWStatusRemote(t, server)
		if strings.Contains(remote.PlanUndetectableReason, bad) {
			t.Errorf("remote caller learned the config path: %q", remote.PlanUndetectableReason)
		}
		if remote.PlanDetectable {
			t.Error("plan_detectable must still be false — it is the honest state, not a disclosure")
		}
	})
}

// TestETWRegisterRefusesWhenProcessCaptureIsOff closes review finding 7: the
// registration endpoint authorised an elevated spawn while the MASTER switch
// [observer.process].enabled was false — the state in which runProcessObserver
// returns early, so the accept listener the task dials into never exists. The
// task would install and reconnect forever against nothing.
func TestETWRegisterRefusesWhenProcessCaptureIsOff(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := etwReviewServer(t, lm, false, true, func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	})
	rec := postETWRegister(t, h, `{}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("register succeeded with [observer.process].enabled = false: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if lm.lastSetupSpec.Label != "" {
		t.Errorf("an elevated PTY was spawned anyway: %+v", lm.lastSetupSpec)
	}
	if !strings.Contains(rec.Body.String(), "[observer.process].enabled") {
		t.Errorf("the refusal must name the master switch; got %s", rec.Body.String())
	}
}

// TestETWStatusMasterSwitchOffIsADisabledSkip is the status half of finding 7:
// with the master switch off there is no listener, so the card must offer to
// turn the feature ON rather than offer to register a task.
func TestETWStatusMasterSwitchOffIsADisabledSkip(t *testing.T) {
	server, _ := etwStatusServer(t, func(c *config.Config) {
		c.Observer.Process.Enabled = false
		c.Observer.Process.ETW.Enabled = true
	})
	server.etwSetupEnvFn = func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	}
	resp := getETWStatus(t, server)
	if resp.State != etwStateSkip || resp.SkipReason != etwSkipDisabled {
		t.Errorf("state=%q skip=%q, want skip/etw_disabled — the feed cannot run with the master switch off",
			resp.State, resp.SkipReason)
	}
	if resp.Command != "" {
		t.Errorf("a command was offered for a feed that cannot run: %q", resp.Command)
	}
	if !resp.SchtasksPresent {
		t.Error("schtasks_present must stay true so the card still renders on a Windows host")
	}
	// The remote projection is flagged even in a state that carries no
	// command: the card's "turn it on" toggle is an owner-local config write,
	// and offering a button that can only 403 is worse than saying so.
	if remote := getETWStatusRemote(t, server); !remote.LocalDetailWithheld {
		t.Error("a remote response must identify itself as the remote-facing projection")
	}
}

// TestETWRegisterDuplicatePOSTDescribesWhatIsActuallyRunning closes review
// finding 8: termsession.Create REUSES a live labelled setup PTY (returning its
// handle with a nil error), so a second POST after a config edit reported the
// NEW plan's command beside the OLD PTY's handle.
func TestETWRegisterDuplicatePOSTDescribesWhatIsActuallyRunning(t *testing.T) {
	lm := &fakeLaunchManager{}
	// The fake returns a CONSTANT handle, which is exactly what termsession does
	// on the reuse path: same label, still-live PTY ⇒ same handle, nil error.
	observer := `/mnt/c/obs/observer.exe`
	h := etwReviewServer(t, lm, true, true, func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, observer)
	})

	first := decodeETWRegister(t, postETWRegister(t, h, `{}`))
	if first.AlreadyRunning {
		t.Fatal("the FIRST spawn must not claim to be already running")
	}
	// The operator edits config between the two POSTs (a different observer.exe).
	observer = `/mnt/c/second/observer.exe`

	second := decodeETWRegister(t, postETWRegister(t, h, `{}`))
	if second.Handle != first.Handle {
		t.Fatalf("precondition: the manager must have REUSED the live PTY (%q vs %q)", second.Handle, first.Handle)
	}
	if second.Command != first.Command {
		t.Errorf("the response described a command that is NOT what the reused PTY is running:\n"+
			" reported: %s\n  running: %s", second.Command, first.Command)
	}
	if !second.AlreadyRunning {
		t.Error("a reused PTY must be reported as already running")
	}
}

// TestETWStatusRemoteRedactionThroughTheRealAuthzChain proves the disclosure
// gate keys off the provenance the REAL remote chain stamps, not off a marker
// only a test sets. A paired View device drives the full
// browserGuard → remoteAuthz → mux assembly.
func TestETWStatusRemoteRedactionThroughTheRealAuthzChain(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.Observer.DBPath = filepath.Join(dir, "observer.db")
	cfg.Observer.Process.Enabled = true
	cfg.Observer.Process.ETW.Enabled = true
	if err := config.WriteToml(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	rc, enc := newReadyRemoteController(t)
	s, err := New(Options{DB: database, DBPath: cfg.Observer.DBPath, ConfigPath: cfgPath, Remote: rc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.etwSetupEnvFn = func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	}
	h := s.remoteGuardedHandler(rc)
	cookie, csrf := pairSession(t, h, enc)

	req := httptest.NewRequest(http.MethodGet, "/api/process/etw/status", nil)
	req.Host = testRemoteHost
	req.AddCookie(cookie)
	req.Header.Set(remoteCSRFHeader, csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("paired View GET = %d, want 200 (the health block is legitimately remote): %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "observer.exe") || strings.Contains(body, "process-bridge-token") ||
		strings.Contains(body, "operator") {
		t.Errorf("the remote response body still carries machine detail: %s", body)
	}
	var resp etwStatusResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.LocalDetailWithheld || resp.State != etwStateManual {
		t.Errorf("want the state ladder with detail withheld, got %+v", resp)
	}
}

// TestETWStatusProbeCacheBoundsLocalExecution closes review finding 11's second
// half: every applicable GET execs schtasks.exe (and cmd.exe for the user
// name), and capability View makes that remote-drivable. The probes must be
// rate-bounded — and MUST NOT be served stale across a config write, which is
// the one moment the card re-reads immediately after changing something.
func TestETWStatusProbeCacheBoundsLocalExecution(t *testing.T) {
	server, _ := etwStatusServer(t, func(c *config.Config) {
		c.Observer.Process.Enabled = true
		c.Observer.Process.ETW.Enabled = true
	})
	var queries, users int
	server.etwSetupEnvFn = func() setup.Env {
		e := fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
		run := e.RunSchtasks
		e.RunSchtasks = func(ctx context.Context, exe string, args ...string) (string, error) {
			queries++
			return run(ctx, exe, args...)
		}
		e.WindowsUser = func() string { users++; return "operator" }
		return e
	}
	now := time.Now()
	server.now = func() time.Time { return now }

	for range 10 {
		if got := getETWStatus(t, server); got.State != etwStateManual {
			t.Fatalf("state = %q, want manual", got.State)
		}
	}
	if queries != 1 || users != 1 {
		t.Errorf("10 polls ran %d schtasks queries and %d username shell-outs; want 1 each within the TTL",
			queries, users)
	}

	// Past the TTL the machine is asked again — this is a rate floor, not a
	// cache of record.
	now = now.Add(etwProbeTTL + time.Second)
	getETWStatus(t, server)
	if queries != 2 {
		t.Errorf("queries = %d after the TTL expired, want a fresh probe", queries)
	}

	// A config write must NEVER be served from the cache: the card's own
	// "turn it on" button writes config and re-reads status immediately.
	before := queries
	cfg := config.Default()
	cfg.Observer.DBPath = server.opts.DBPath
	cfg.Observer.Process.Enabled = true
	cfg.Observer.Process.ETW.Enabled = true
	cfg.Observer.Process.ETW.ListenAddr = "127.0.0.1:9999"
	if err := config.WriteToml(server.opts.ConfigPath, cfg); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	got := getETWStatus(t, server)
	if queries != before+1 {
		t.Errorf("queries = %d, want a re-probe after the config changed", queries)
	}
	if got.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("listen_addr = %q, want the freshly-written value", got.ListenAddr)
	}

	// A request whose CALLER went away mid-probe answers honestly for itself
	// but must not poison the next five seconds for everyone else.
	now = now.Add(etwProbeTTL + time.Second) // past the TTL, so this one really probes
	before = queries
	req := httptest.NewRequest(http.MethodGet, "/api/process/etw/status", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	server.Handler().ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	if queries != before+1 {
		t.Fatalf("precondition: the cancelled request should still have attempted a probe (%d → %d)", before, queries)
	}
	getETWStatus(t, server)
	if queries != before+2 {
		t.Errorf("queries = %d: a cancelled request's answer was cached", queries)
	}
}

func decodeETWRegister(t *testing.T, rec *httptest.ResponseRecorder) etwRegisterResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp etwRegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return resp
}
