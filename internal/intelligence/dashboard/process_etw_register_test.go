package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/processbridge/setup"
)

// process_etw_register_test.go covers POST /api/process/etw/register — the
// elevation broker of the dashboard-driven ETW capturer setup (plan §E3).
//
// Every test injects the setup.Env probe seam, so NO test ever reaches the real
// Windows Task Scheduler (the browserhost-tests-hit-the-real-registry class of
// mistake), and a CI host that happens to have interop cannot change an answer.

// etwRegisterServer assembles a Server with the confirm-token machinery (a
// ready Remote controller), a config on disk, a fake launch manager, and the
// injected probe seam. Returns the handler plus the manager so a test can
// inspect exactly what was — or was not — spawned.
func etwRegisterServer(t *testing.T, lm LaunchManager, etwEnabled bool, env func() setup.Env) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.Observer.DBPath = filepath.Join(dir, "observer.db")
	cfg.Observer.Process.Enabled = true
	cfg.Observer.Process.ETW.Enabled = etwEnabled
	if err := config.WriteToml(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	database, err := openTestDB(context.Background(), db.Options{Path: cfg.Observer.DBPath})
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
		env = func() setup.Env { return setup.Env{} } // no schtasks.exe
	}
	s.etwSetupEnvFn = env
	return s.Handler()
}

// postETWRegister issues a confirm-gated POST with the given raw body.
func postETWRegister(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	ck, ctok := getConfirm(t, h)
	req := httptest.NewRequest(http.MethodPost, "/api/process/etw/register", bytes.NewReader([]byte(body)))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, ctok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// etwNotFoundOut / etwNotFound are what a `schtasks /Query` for a MISSING task
// actually prints and exits with (measured 2026-07-26). The output matters:
// setup.probeTask maps only this wording to ProbeAbsent, and anything else to
// the deliberately-hedged ProbeUnknown.
const etwNotFoundOut = "ERROR: The system cannot find the file specified."

var etwNotFound = errors.New("exit status 1")

const etwWindowsObserver = `/mnt/c/obs/observer.exe`

// TestETWRegisterSpawnsPlannerArgv is the core assertion: the spawned argv is
// EXACTLY setup.ElevatedRegisterArgv over the planner's own resolved schtasks
// args, and the setup label is the route's own single-flight key.
func TestETWRegisterSpawnsPlannerArgv(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := etwRegisterServer(t, lm, true, func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	})
	rec := postETWRegister(t, h, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp etwRegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if resp.Handle == "" {
		t.Error("response must carry the PTY handle")
	}
	if resp.TaskName != setup.TaskName {
		t.Errorf("task_name = %q, want %q", resp.TaskName, setup.TaskName)
	}
	if !resp.UACRequired {
		t.Error("uac_required must be stated, not implied")
	}
	if resp.PlanState != etwStateManual {
		t.Errorf("plan_state = %q, want %q", resp.PlanState, etwStateManual)
	}
	if lm.lastSetupSpec.Label != etwRegisterLabel {
		t.Errorf("setup label = %q, want %q (the single-flight key)", lm.lastSetupSpec.Label, etwRegisterLabel)
	}

	argv := lm.lastSetupSpec.Argv
	if len(argv) != 5 {
		t.Fatalf("argv has %d elements, want 5 (powershell + 3 flags + script): %#v", len(argv), argv)
	}
	if argv[0] != setup.PowerShellExe || argv[1] != "-NoProfile" ||
		argv[2] != "-NonInteractive" || argv[3] != "-Command" {
		t.Errorf("argv prefix = %#v, want the fixed powershell.exe -NoProfile -NonInteractive -Command", argv[:4])
	}
	script := argv[4]
	// The elevation itself.
	if !strings.Contains(script, "-Verb RunAs") {
		t.Error("script must elevate via -Verb RunAs — that IS the UAC consent step")
	}
	// The measured quoting reaches the child untouched: the script must embed
	// the planner's own schtasks args, single-quoted /TR and all.
	if !strings.Contains(resp.Command, `/TR "'`) {
		t.Errorf("the command must keep the measured single-quoted /TR form: %s", resp.Command)
	}
	tail := strings.TrimPrefix(resp.Command, setup.SchtasksExe+" ")
	if tail == resp.Command {
		t.Fatalf("command does not start with %q: %s", setup.SchtasksExe, resp.Command)
	}
	if !strings.Contains(script, setup.ElevatedRegisterArgv(tail)[4]) {
		t.Errorf("the spawned script is not ElevatedRegisterArgv over the reported command tail:\nscript=%s", script)
	}
	// And the whole argv must be reproducible from the reported command alone —
	// i.e. the broker used the planner's string, not one of its own.
	if !reflect.DeepEqual(argv, setup.ElevatedRegisterArgv(tail)) {
		t.Errorf("spawned argv is not ElevatedRegisterArgv(plan.SchtasksArgs)")
	}
}

// TestETWRegisterIgnoresRequestBody is the injection floor: NOTHING in the
// request may reach argv. Two POSTs whose bodies differ wildly — one empty, one
// stuffed with every field name the handler could plausibly have read — must
// spawn byte-identical argv.
func TestETWRegisterIgnoresRequestBody(t *testing.T) {
	poison := `{"argv":["calc.exe"],"command":"schtasks /Create /TN pwn /TR calc.exe",` +
		`"task_name":"pwn","schtasks_args":"/Create /TN pwn","label":"pwn",` +
		`"exe":"C:\\evil.exe","token_path":"C:\\evil","listen_addr":"1.2.3.4:9","notes":["x"]}`

	// ONE server for both POSTs: the token path is derived from the config's DB
	// directory, so two servers would differ for a reason that has nothing to do
	// with the request body and would make this assertion meaningless.
	lm := &fakeLaunchManager{}
	h := etwRegisterServer(t, lm, true, func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	})
	var argvs [][]string
	for _, body := range []string{`{}`, poison} {
		rec := postETWRegister(t, h, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("register(%s) = %d: %s", body, rec.Code, rec.Body.String())
		}
		argvs = append(argvs, lm.lastSetupSpec.Argv)
	}
	if !reflect.DeepEqual(argvs[0], argvs[1]) {
		t.Fatalf("request body changed the argv — the body must never be read:\nclean=%#v\npoisoned=%#v",
			argvs[0], argvs[1])
	}
	joined := strings.Join(argvs[1], " ")
	for _, bad := range []string{"calc.exe", "pwn", "evil", "1.2.3.4"} {
		if strings.Contains(joined, bad) {
			t.Errorf("request-supplied %q reached the argv: %s", bad, joined)
		}
	}
}

// TestETWRegisterRefusals walks every state that must NOT spawn a privileged
// PTY, and pins that each refusal names the thing the operator must change.
func TestETWRegisterRefusals(t *testing.T) {
	tests := []struct {
		name       string
		etwEnabled bool
		env        setup.Env
		wantCode   int
		reasonHas  string
	}{
		{
			name:       "etw disabled names the config key",
			etwEnabled: false,
			env:        fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver),
			wantCode:   http.StatusBadRequest,
			reasonHas:  "[observer.process.etw].enabled",
		},
		{
			name:       "no schtasks.exe says this is not a Windows host",
			etwEnabled: true,
			env:        setup.Env{},
			wantCode:   http.StatusBadRequest,
			reasonHas:  "schtasks.exe",
		},
		{
			name:       "an existing task is a 409 that leaves it untouched",
			etwEnabled: true,
			env:        fakeSchtasksEnv("TaskName: "+setup.TaskName, nil, etwWindowsObserver),
			wantCode:   http.StatusConflict,
			reasonHas:  "already registered",
		},
		{
			name:       "a blocked plan names the missing dependency",
			etwEnabled: true,
			env:        fakeSchtasksEnv(etwNotFoundOut, etwNotFound, ""), // no Windows observer.exe
			wantCode:   http.StatusBadRequest,
			reasonHas:  "windows_binary_path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lm := &fakeLaunchManager{}
			h := etwRegisterServer(t, lm, tc.etwEnabled, func() setup.Env { return tc.env })
			rec := postETWRegister(t, h, `{}`)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.reasonHas) {
				t.Errorf("refusal must name %q: %s", tc.reasonHas, rec.Body.String())
			}
			if lm.lastSetupSpec.Argv != nil {
				t.Errorf("a refused precondition must spawn NOTHING, got %#v", lm.lastSetupSpec.Argv)
			}
		})
	}
}

// TestETWRegisterBlockedNamesThePathTried pins the §W4.6 #6 fix all the way to
// the wire: when a Windows binary path WAS configured and does not exist, the
// refusal must quote that path — telling an operator to set a key they already
// set is a dead end.
func TestETWRegisterBlockedNamesThePathTried(t *testing.T) {
	lm := &fakeLaunchManager{}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.Observer.DBPath = filepath.Join(dir, "observer.db")
	cfg.Observer.Process.Enabled = true
	cfg.Observer.Process.ETW.Enabled = true
	cfg.Observer.Process.WindowsBinaryPath = `/mnt/c/nope/observer.exe`
	if err := config.WriteToml(cfgPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	database, err := openTestDB(context.Background(), db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	rc, _ := newReadyRemoteController(t)
	s, err := New(Options{DB: database, DBPath: cfg.Observer.DBPath, ConfigPath: cfgPath, Remote: rc, LaunchManager: lm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.etwSetupEnvFn = func() setup.Env { return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, "") }

	rec := postETWRegister(t, s.Handler(), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blocked = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/mnt/c/nope/observer.exe") {
		t.Errorf("the blocked reason must name the PATH it tried: %s", rec.Body.String())
	}
	if lm.lastSetupSpec.Argv != nil {
		t.Error("a blocked plan must spawn nothing")
	}
}

// TestETWRegisterUnknownProbeStillSpawns pins that a probe that could not
// answer is NOT treated as "already present": the command is still offered
// (without /F schtasks refuses to overwrite), and plan_state carries the hedge
// so the card can keep saying so.
func TestETWRegisterUnknownProbeStillSpawns(t *testing.T) {
	lm := &fakeLaunchManager{}
	h := etwRegisterServer(t, lm, true, func() setup.Env {
		return fakeSchtasksEnv("ERROR: Access is denied.", errors.New("exit status 1"), etwWindowsObserver)
	})
	rec := postETWRegister(t, h, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown-probe register = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp etwRegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PlanState != etwStateUnknown {
		t.Errorf("plan_state = %q, want %q (the hedge must survive to the wire)", resp.PlanState, etwStateUnknown)
	}
	if lm.lastSetupSpec.Argv == nil {
		t.Error("an unknown probe must still offer the registration")
	}
}

// TestETWRegisterSpawnBoundaryRecheck is the TOCTOU pin. The probe seam flips
// to "the task exists" AFTER the advisory pass has already seen it absent; the
// spawn-boundary re-check must catch it, answer 409, and spawn nothing. Without
// the second pass this test spawns a privileged PTY that would try to create a
// task that already exists.
func TestETWRegisterSpawnBoundaryRecheck(t *testing.T) {
	lm := &fakeLaunchManager{}
	var calls int
	h := etwRegisterServer(t, lm, true, func() setup.Env {
		calls++
		if calls <= 1 {
			return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver) // absent
		}
		return fakeSchtasksEnv("TaskName: "+setup.TaskName, nil, etwWindowsObserver) // appeared
	})
	rec := postETWRegister(t, h, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("raced registration = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if calls < 2 {
		t.Errorf("the plan was resolved %d time(s) — the spawn boundary must re-check, not reuse the advisory pass", calls)
	}
	if lm.lastSetupSpec.Argv != nil {
		t.Errorf("a task that appeared mid-request must not be clobbered, got spawn %#v", lm.lastSetupSpec.Argv)
	}
}

// TestETWRegisterInFlight pins the 409 single-flight passthrough: a second POST
// while a privileged PTY of this kind is already starting is refused, so button
// spam cannot queue up UAC prompts.
func TestETWRegisterInFlight(t *testing.T) {
	lm := &fakeLaunchManager{setupErr: ErrLaunchSetupInFlight}
	h := etwRegisterServer(t, lm, true, func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	})
	rec := postETWRegister(t, h, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("in-flight register = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestETWRegisterNoLaunchManager pins the honest 503 when no PTY can be hosted,
// with a message that points at the manual command rather than failing blank.
func TestETWRegisterNoLaunchManager(t *testing.T) {
	h := etwRegisterServer(t, nil, true, func() setup.Env {
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	})
	rec := postETWRegister(t, h, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil manager = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "yourself") {
		t.Errorf("the 503 must point at the manual path: %s", rec.Body.String())
	}
}

// TestETWRegisterConfirmAndMethod pins the CSRF/method hardening, and — the
// part that matters most here — that a caller who fails it never reaches a
// probe, let alone a spawn.
func TestETWRegisterConfirmAndMethod(t *testing.T) {
	lm := &fakeLaunchManager{}
	var probes int
	h := etwRegisterServer(t, lm, true, func() setup.Env {
		probes++
		return fakeSchtasksEnv(etwNotFoundOut, etwNotFound, etwWindowsObserver)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/process/etw/register", bytes.NewReader([]byte(`{}`)))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("missing confirm token must not succeed: %s", rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/process/etw/register", nil)
	req2.Host = "127.0.0.1:8080"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET register = %d, want 405", rec2.Code)
	}

	if probes != 0 {
		t.Errorf("a rejected request ran %d probe(s) — the confirm gate must come first", probes)
	}
	if lm.lastSetupSpec.Argv != nil {
		t.Error("a rejected request must spawn nothing")
	}
}
