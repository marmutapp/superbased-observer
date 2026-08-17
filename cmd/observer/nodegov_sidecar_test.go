package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/govern"
	"github.com/marmutapp/superbased-observer/internal/govern/sidecar"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/policyfam"
	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
	"github.com/marmutapp/superbased-observer/internal/policystate"
)

func sidecarQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sidecarTestSpec(t *testing.T, body string) nodegov.PolicySpec {
	t.Helper()
	spec, _, err := nodegov.CompileBody([]byte(body), 1<<20)
	if err != nil {
		t.Fatalf("CompileBody(%s): %v", body, err)
	}
	return spec
}

func sidecarTestGrant(now time.Time) *govern.Grant {
	return &govern.Grant{
		OrgKey: "ok", Generation: 2, OrgName: "Acme", KeyPinSHA256: "pin",
		Authority: []string{
			govern.AuthorityDashboardVisibility,
			govern.AuthoritySettingsPin,
			govern.AuthorityCapturePin,
			govern.AuthorityFeatureLock,
		},
		GrantedAt: now.Add(-time.Hour), ExpiresAt: now.Add(30 * 24 * time.Hour),
	}
}

func sidecarTestHandle(t *testing.T, dir, body string, startup startupSidecar) (*nodeGovernanceHandle, string) {
	t.Helper()
	now := time.Now().UTC()
	h := newNodeGovernanceHandle(func(context.Context) (*govern.Grant, govern.LiveIdentity, error) {
		return sidecarTestGrant(now), govern.LiveIdentity{Enrolled: true, OrgKey: "ok", Generation: 2, KeyPinSHA256: "pin"}, nil
	}, sidecarQuietLogger())
	path := filepath.Join(dir, config.GovernanceSidecarFilename)
	h.SetSidecar(path, "test", startup)
	if body != "" {
		h.Apply(orgclient.PolicyResourceResult{
			Status: orgclient.PRApplied, Family: policyfam.FamilyNodeGovernance,
			Version: 14, BodyHash: "bh", EnforceAllowed: true,
			Spec: sidecarTestSpec(t, body),
		})
	}
	return h, path
}

// TestSidecarWriterMaterializesPins: the whole point of the file. A pin the
// daemon resolved must be readable by config.Load, in another process, with
// only a path.
func TestSidecarWriterMaterializesPins(t *testing.T) {
	dir := t.TempDir()
	h, path := sidecarTestHandle(t, dir, `{"schema":2,"pinned":{"guard.enabled":true}}`, startupSidecar{})
	if err := h.SidecarWriteErr(); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	f, reason := sidecar.Read(path, time.Now())
	if f == nil {
		t.Fatalf("no sidecar was written: %q", reason)
	}
	if f.Pinned["guard.enabled"] != true {
		t.Fatalf("pinned = %+v", f.Pinned)
	}
	if f.State != sidecar.StateApplied {
		t.Fatalf("state = %q", f.State)
	}
}

// TestSoloNodeWritesNoSidecarFile: an UNGRANTED node creates nothing.
// "Dormant is written, not deleted" is about a node that WAS governed; §8's
// solo claim is literally no file, no new default, no new write.
func TestSoloNodeWritesNoSidecarFile(t *testing.T) {
	dir := t.TempDir()
	h := newNodeGovernanceHandle(func(context.Context) (*govern.Grant, govern.LiveIdentity, error) {
		return nil, govern.LiveIdentity{}, nil
	}, sidecarQuietLogger())
	path := filepath.Join(dir, config.GovernanceSidecarFilename)
	h.SetSidecar(path, "test", startupSidecar{})
	h.WriteSidecar(context.Background())
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a solo node created a governance sidecar")
	}
}

// TestGovernedNodeGoingDormantRewritesTheFile: once a file exists, the
// daemon converges it to a DORMANT record rather than deleting it —
// presence-with-empty is unambiguous, absence is indistinguishable from "the
// daemon never ran".
func TestGovernedNodeGoingDormantRewritesTheFile(t *testing.T) {
	dir := t.TempDir()
	h, path := sidecarTestHandle(t, dir, `{"schema":2,"pinned":{"guard.enabled":true}}`, startupSidecar{})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no sidecar to converge: %v", err)
	}
	// The grant vanishes (an unenrol landing from another process).
	h.SetIdentityLoader(func(context.Context) (*govern.Grant, govern.LiveIdentity, error) {
		return nil, govern.LiveIdentity{}, nil
	})
	h.WriteSidecar(context.Background())

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file was deleted rather than converged: %v", err)
	}
	f, derr := sidecar.Decode(raw)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if !f.Dormant() || len(f.Pinned) != 0 {
		t.Fatalf("dormant file still carries directives: %+v", f)
	}
	if _, reason := sidecar.Read(path, time.Now()); reason != sidecar.ReasonNotApplied {
		t.Fatalf("a reader still honours the dormant file: %q", reason)
	}
}

// TestSidecarWriteFailureForcesInert is review B3: without it, a node whose
// ~/.observer is read-only resolves StateApplied, returns an empty
// InertReason, emits EnforceMode "enforce" — and acks EFFECTIVE while no
// process on the machine can read the pins. That is the false compliance
// claim Phase 1b exists to remove, re-entering through the back door.
func TestSidecarWriteFailureForcesInert(t *testing.T) {
	dir := t.TempDir()
	// A path whose PARENT is a regular file: MkdirAll fails, so every write
	// attempt fails, on every platform.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, _ := sidecarTestHandle(t, filepath.Join(blocker, "nested"), `{"schema":2,"pinned":{"guard.enabled":true}}`, startupSidecar{})

	if h.SidecarWriteErr() == nil {
		t.Fatal("the write did not fail — the fixture proves nothing")
	}
	facts := h.Facts(context.Background())
	if facts.InertReason == "" {
		t.Fatal("a node that cannot write its effective-settings file reported NO inert reason — it would ack `effective` while no process can read the pins")
	}
	// And the reason on the wire stays inside the server's closed set.
	if facts.InertReason != "not_preauthorized" {
		t.Fatalf("InertReason = %q — the server 400s the WHOLE report on an unknown reason, so this must stay not_preauthorized until the Phase-2 wire item ships", facts.InertReason)
	}
}

// TestReadOnlyObserverDirDoesNotReportEffective is the same claim through
// the P0-6 point reader, which is what actually reaches the org.
func TestReadOnlyObserverDirDoesNotReportEffective(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode bits do not gate writes the same way on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not block writes")
	}
	dir := t.TempDir()
	roDir := filepath.Join(dir, "observer")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	h, _ := sidecarTestHandle(t, roDir, `{"schema":2,"pinned":{"guard.enabled":true}}`, startupSidecar{})
	if h.SidecarWriteErr() == nil {
		t.Skip("this filesystem allowed the write into a 0500 directory")
	}
	reader := newNodeGovernancePointReader(h, nil, time.Now)
	facts, err := reader(context.Background())
	if err != nil {
		t.Fatalf("point reader: %v", err)
	}
	if facts.EnforceMode != "observe" || facts.InertReason == "" {
		t.Fatalf("point facts = %+v, want observe/inert — a node that cannot materialize the posture must not report enforce", facts)
	}
}

// TestPinnedChangeReportsPendingRestart is review M3: the shipped Phase-1a
// point reader set CachedAcceptedVersion and RunningVersion to the SAME
// g.Version in both branches, so as-shipped the node would report
// `effective` the instant it wrote a pinned map the running process had
// never read.
func TestPinnedChangeReportsPendingRestart(t *testing.T) {
	dir := t.TempDir()
	// The running process started with NO sidecar at all.
	h, _ := sidecarTestHandle(t, dir, `{"schema":2,"pinned":{"guard.enabled":true}}`, startupSidecar{})
	reader := newNodeGovernancePointReader(h, nil, time.Now)
	facts, err := reader(context.Background())
	if err != nil {
		t.Fatalf("point reader: %v", err)
	}
	if facts.CachedAcceptedVersion != 14 {
		t.Fatalf("cached = %d, want the delivered 14", facts.CachedAcceptedVersion)
	}
	if facts.RunningVersion >= facts.CachedAcceptedVersion {
		t.Fatalf("running = %d, cached = %d — pending_restart is derived from running < cached, so this node would overclaim `effective`",
			facts.RunningVersion, facts.CachedAcceptedVersion)
	}
	// The collector's own rule (R3-B2): a zero running version means an
	// empty hash on the org-rail row — so a pending_restart node can never
	// present the delivered body's hash as if it were already running.
	row := policystate.Resolve(policystate.PointNodeDashboard, "node.governance", facts)
	if !row.RestartRequired {
		t.Fatalf("row = %+v, want RestartRequired for running < cached", row)
	}
	if row.EffectiveHash != "" {
		t.Fatalf("row.EffectiveHash = %q, want empty while RunningVersion is 0", row.EffectiveHash)
	}
}

// TestRestartConvergesToEffective: once the daemon restarts and its own
// config.Load has consumed the sidecar, running == cached and the node
// reports effective.
func TestRestartConvergesToEffective(t *testing.T) {
	dir := t.TempDir()
	body := `{"schema":2,"pinned":{"guard.enabled":true}}`

	// First run writes the file.
	first, path := sidecarTestHandle(t, dir, body, startupSidecar{})
	if err := first.SidecarWriteErr(); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The "restart": a fresh process loads config, consuming that sidecar.
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[observer]\ndb_path = \""+
		filepath.ToSlash(filepath.Join(dir, "observer.db"))+"\"\n[guard]\nenabled = false\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, gout, err := config.LoadGovernance(config.LoadOptions{GlobalPath: cfgPath, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("LoadGovernance: %v", err)
	}
	if !cfg.Guard.Enabled {
		t.Fatal("the restarted process did not read the pin")
	}
	_ = path

	second, _ := sidecarTestHandle(t, dir, body, startupSidecarFrom(gout))
	facts, err := newNodeGovernancePointReader(second, nil, time.Now)(context.Background())
	if err != nil {
		t.Fatalf("point reader: %v", err)
	}
	if facts.RunningVersion != facts.CachedAcceptedVersion || facts.InertReason != "" {
		t.Fatalf("after a restart the node still reports %+v, want running == cached and no inert reason", facts)
	}
	if facts.EnforceMode != "enforce" {
		t.Fatalf("EnforceMode = %q, want enforce", facts.EnforceMode)
	}
}

// TestSectionsOnlyChangeStaysHot: sections and share are HOT, so a body
// changing only those converges to `effective` without a restart. Only a
// changed PINNED map moves the point.
func TestSectionsOnlyChangeStaysHot(t *testing.T) {
	dir := t.TempDir()
	h, _ := sidecarTestHandle(t, dir, `{"schema":1,"sections":{"hidden":["benchmarks"]}}`, startupSidecar{})
	facts, err := newNodeGovernancePointReader(h, nil, time.Now)(context.Background())
	if err != nil {
		t.Fatalf("point reader: %v", err)
	}
	if facts.RunningVersion != facts.CachedAcceptedVersion {
		t.Fatalf("a sections-only body reported pending_restart (%+v) — hiding a page mutates the live surface immediately", facts)
	}
}

// TestHookFailsOpenOnMalformedSidecar drives the REAL hook binary against a
// range of hostile sidecars. A non-zero PreToolUse exit BLOCKS the
// developer's tool call, so this is the fleet-wide blast-radius case and it
// cannot be proven by a unit test that never spawns a hook process.
func TestHookFailsOpenOnMalformedSidecar(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the observer binary")
	}
	bin := filepath.Join(t.TempDir(), "observer-hook-test")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	bodies := map[string]string{
		"truncated":       `{"schema":1,`,
		"oversize":        `{"schema":1,"state":"applied","org_name":"` + string(make([]byte, 70<<10)) + `"}`,
		"wrong schema":    `{"schema":42,"state":"applied"}`,
		"unknown field":   `{"schema":1,"state":"applied","nope":1}`,
		"validate reject": `{"schema":1,"state":"applied","pinned":{"guard.mode":"strict"}}`,
		"expired grant":   `{"schema":1,"state":"applied","grant_expires_at":"2000-01-01T00:00:00Z","pinned":{"guard.enabled":true}}`,
		"not json":        `absolutely not json`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			obsDir := filepath.Join(home, ".observer")
			if err := os.MkdirAll(obsDir, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(obsDir, config.GovernanceSidecarFilename), []byte(body), 0o600); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
			cmd := exec.Command(bin, "hook", "claude-code", "PreToolUse")
			cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
			cmd.Stdin = readerOf(`{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/tmp/x"},"cwd":"/tmp"}`)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the hook FAILED against a %s sidecar (%v) — a non-zero PreToolUse exit blocks the developer's tool call:\n%s", name, err, out)
			}
			if containsGovernanceNoise(string(out)) {
				t.Fatalf("the hook wrote about governance to stderr:\n%s", out)
			}
		})
	}
}

func readerOf(s string) *os.File {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	go func() {
		_, _ = w.WriteString(s)
		_ = w.Close()
	}()
	return r
}

// containsGovernanceNoise reports whether hook output mentions the
// governance machinery. Several AI clients surface hook stderr to the
// developer on EVERY tool call, so a governance diagnostic there would be a
// fleet-wide annoyance with no corresponding benefit.
func containsGovernanceNoise(out string) bool {
	for _, needle := range []string{"governance-effective", "sidecar", "govern:"} {
		if strings.Contains(out, needle) {
			return true
		}
	}
	return false
}
