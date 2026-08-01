package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateHelperEnv puts TestLiveDBGate_EscapeHatchIsCommandLineOnly into its
// child-process mode. See that test for why a subprocess is required.
const gateHelperEnv = "OBSERVER_LIVE_DB_GATE_HELPER"

// pinGuardedRoots re-points the package's captured "real ~/.observer" set at
// temp directories for the duration of one test, and restores it after.
//
// This is what makes the behavioural proofs below SAFE WHEN RED: they exercise
// the incident shape — a test handing db.Open the path the gate is supposed to
// refuse — against a stand-in directory. If someone deletes the gate, the
// assertions fail and the only artifact is a SQLite file in a t.TempDir().
// Pointing them at the genuine ~/.observer would mean a deleted gate migrates
// the operator's production database, i.e. re-running the incident to prove
// the incident. The invariant suite (tests/invariant/live_db_gate_test.go)
// pins the OTHER half — that the captured value really is the operator's home.
func pinGuardedRoots(t *testing.T, roots ...string) {
	t.Helper()
	prev := guardedRoots
	t.Cleanup(func() { guardedRoots = prev })
	guardedRoots = append([]string(nil), roots...)
}

// newFakeRealDir makes a stand-in for the operator's ~/.observer and pins the
// gate to it.
func newFakeRealDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".observer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pinGuardedRoots(t, dir)
	return dir
}

// TestLiveDBGate_OpenRefusesRealDataDir is the regression test for incident
// "task #17" (2026-07-30): a test resolved config.Default()'s
// `~/.observer/observer.db` and ran agent migration 078 against the operator's
// live database.
//
// It reproduces the incident shape exactly — Open on `<real>/observer.db` from
// a test binary — and requires that Open refuse with ErrRealDBInTest, create
// nothing, and never reach the migration runner. Delete the guard call in
// Open and this test fails on the "wanted ErrRealDBInTest" branch.
func TestLiveDBGate_OpenRefusesRealDataDir(t *testing.T) {
	fakeReal := newFakeRealDir(t)

	// Every spelling a careless test might use, including the sibling files
	// SQLite writes next to the database and a ".." escape.
	paths := []string{
		filepath.Join(fakeReal, "observer.db"),
		filepath.Join(fakeReal, "observer.db-wal"),
		filepath.Join(fakeReal, "nested", "scratch.db"),
		fakeReal,
		// Un-cleaned spelling: filepath.Join would normalise this away, so
		// build it by hand to prove the gate cleans before comparing.
		fakeReal + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "observer.db",
	}
	for _, p := range paths {
		database, err := Open(context.Background(), Options{Path: p})
		if err == nil {
			_ = database.Close()
			t.Fatalf("Open(%q) SUCCEEDED — the live-DB test gate is gone (incident task #17)", p)
		}
		if !errors.Is(err, ErrRealDBInTest) {
			t.Fatalf("Open(%q) error = %v, want ErrRealDBInTest", p, err)
		}
		if !strings.Contains(err.Error(), AllowRealDBInTestEnv) {
			t.Errorf("Open(%q) error does not name the escape hatch %s: %v", p, AllowRealDBInTestEnv, err)
		}
		if _, statErr := os.Stat(p); statErr == nil && p != fakeReal {
			t.Errorf("Open(%q) created a file despite refusing — the gate must sit above sql.Open", p)
		}
	}
}

// TestLiveDBGate_AliasedPathsAreRefused is the regression test for codex
// FINDING 1 (HIGH). The first cut of the path check resolved only
// filepath.Dir(abs) and re-attached the ORIGINAL basename, so a symlink in the
// FINAL component — `/tmp/live.db -> ~/.observer/observer.db` — matched no
// guarded root and walked straight through Open into runMigrations against the
// live file. Confirmed empirically before the fix; this pins it shut.
//
// The hard-link case closes the inode-alias sibling: a hard link has no target
// to resolve AND no shared path prefix, so only os.SameFile can see it.
func TestLiveDBGate_AliasedPathsAreRefused(t *testing.T) {
	fakeReal := newFakeRealDir(t)
	live := filepath.Join(fakeReal, "observer.db")
	if err := os.WriteFile(live, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	cases := []struct {
		name string
		make func(t *testing.T, target string) string
	}{
		{"symlink in the FINAL component", func(t *testing.T, target string) string {
			p := filepath.Join(outside, "live-symlink.db")
			if err := os.Symlink(target, p); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return p
		}},
		// This case ISOLATES the finding-1 fix. The three cases around it are
		// also caught by the os.SameFile inode check (os.Stat follows
		// symlinks), so reverting the full-path EvalSymlinks alone leaves them
		// green — verified by mutation. A symlink whose target is a
		// non-liveDBBasenames file inside the guarded directory has no inode
		// to match, so ONLY full-path symlink resolution can see it.
		{"symlink in the FINAL component to a NON-live-named file", func(t *testing.T, target string) string {
			other := filepath.Join(filepath.Dir(target), "staged-import.db")
			if err := os.WriteFile(other, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(outside, "staged-symlink.db")
			if err := os.Symlink(other, p); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return p
		}},
		{"symlink to the live DIRECTORY", func(t *testing.T, target string) string {
			d := filepath.Join(outside, "dirlink")
			if err := os.Symlink(filepath.Dir(target), d); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return filepath.Join(d, "observer.db")
		}},
		{"hard link to the live file", func(t *testing.T, target string) string {
			p := filepath.Join(outside, "live-hardlink.db")
			if err := os.Link(target, p); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			return p
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.make(t, live)

			// Assert at the PATH seam first, and separately from Open. This
			// is what isolates the finding-1 fix: SQLite resolves symlinks
			// itself, so PRAGMA database_list reports the canonical path and
			// the handle-form guard inside runMigrations would catch these
			// cases even with the path check reverted — but only AFTER
			// sql.Open has touched (and WAL-locked) the real file. Asserting
			// GuardLiveDB directly proves the refusal happens before any
			// syscall against the live database.
			if err := GuardLiveDB(p); !errors.Is(err, ErrRealDBInTest) {
				t.Fatalf("GuardLiveDB(%q) = %v, want ErrRealDBInTest — a %s reached the live database "+
					"without the PATH seam noticing (codex finding 1)", p, err, c.name)
			}

			database, err := Open(context.Background(), Options{Path: p})
			if err == nil {
				_ = database.Close()
				t.Fatalf("Open(%q) SUCCEEDED — a %s aliases the live database and bypassed the gate", p, c.name)
			}
			if !errors.Is(err, ErrRealDBInTest) {
				t.Fatalf("Open(%q) error = %v, want ErrRealDBInTest", p, err)
			}
			// Refusal must be total: no WAL/SHM sidecars next to the live file.
			for _, sfx := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(live + sfx); err == nil {
					t.Errorf("Open(%q) created %q — the live database was opened before being refused", p, live+sfx)
				}
			}
		})
	}
}

// TestLiveDBGate_MigrationRunnerGuardsItself is the regression test for codex
// FINDING 3 (MEDIUM). migrations_test.go builds a raw database/sql handle and
// calls runMigrations directly, so guarding only db.Open left the DAMAGE site
// reachable. runMigrations must refuse a handle attached to the live file
// regardless of how that handle was obtained.
func TestLiveDBGate_MigrationRunnerGuardsItself(t *testing.T) {
	// Open a handle to a path that is NOT yet guarded, so the raw open
	// succeeds, then pin the gate over it — precisely "a handle that reached
	// the runner without passing the path seam".
	dir := filepath.Join(t.TempDir(), ".observer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "observer.db")
	database, err := sql.Open("sqlite", "file:"+p+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := database.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	pinGuardedRoots(t, dir)
	if err := runMigrations(context.Background(), database); !errors.Is(err, ErrRealDBInTest) {
		t.Fatalf("runMigrations on a live-DB handle = %v, want ErrRealDBInTest — the damage site is unguarded", err)
	}
	if err := GuardLiveDBHandle(context.Background(), database); !errors.Is(err, ErrRealDBInTest) {
		t.Fatalf("GuardLiveDBHandle = %v, want ErrRealDBInTest", err)
	}
}

// TestLiveDBGate_SandboxPathsStillOpen is the no-false-positive pin: the
// legitimate shapes every test in this tree uses (t.TempDir(), ":memory:", and
// a FAKED home whose own .observer lives inside the sandbox) must still open
// and migrate normally. A gate that blocked these would be worse than the bug.
func TestLiveDBGate_SandboxPathsStillOpen(t *testing.T) {
	fakeReal := newFakeRealDir(t)

	sandboxHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sandboxHome, ".observer"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		":memory:",
		filepath.Join(t.TempDir(), "observer.db"),
		// The correct pattern: HOME faked to a sandbox, so the sandbox's own
		// ".observer/observer.db" is NOT the operator's.
		filepath.Join(sandboxHome, ".observer", "observer.db"),
		// A sibling directory that merely shares a prefix must not match.
		filepath.Join(filepath.Dir(fakeReal), ".observer-backup", "observer.db"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); p != ":memory:" && err != nil {
			t.Fatal(err)
		}
		database, err := Open(context.Background(), Options{Path: p})
		if err != nil {
			t.Fatalf("Open(%q) = %v, want success (the gate must not block sandboxed tests)", p, err)
		}
		_ = database.Close()
	}
}

// TestLiveDBGate_SetenvCannotOpenTheHatch is the regression test for codex
// FINDING 4 (MEDIUM). The first cut read AllowRealDBInTestEnv per call, so
// t.Setenv/os.Setenv from test code re-opened the hole the gate exists to
// close. The value is now captured once at package init, before any test runs,
// so setting it programmatically must be inert.
func TestLiveDBGate_SetenvCannotOpenTheHatch(t *testing.T) {
	if allowRealDB {
		t.Skipf("%s was set on the command line for this run; the hatch is legitimately open", AllowRealDBInTestEnv)
	}
	fakeReal := newFakeRealDir(t)
	t.Setenv(AllowRealDBInTestEnv, "1")

	p := filepath.Join(fakeReal, "observer.db")
	database, err := Open(context.Background(), Options{Path: p})
	if err == nil {
		_ = database.Close()
		t.Fatalf("t.Setenv(%s) opened the escape hatch — it must be command-line-only", AllowRealDBInTestEnv)
	}
	if !errors.Is(err, ErrRealDBInTest) {
		t.Fatalf("Open error = %v, want ErrRealDBInTest", err)
	}
}

// TestLiveDBGate_EscapeHatchIsCommandLineOnly proves the documented opt-in
// actually works — and can ONLY work — when the env var is present in the
// environment that SPAWNED the test binary.
//
// It has to re-exec itself: allowRealDB and guardedRoots are captured at
// package init, so neither branch can be exercised in-process. The child gets
// HOME pointed at a sandbox (making <sandbox>/.observer a guarded root, so the
// child never goes near the operator's data) and, in one of the two runs, the
// hatch variable.
func TestLiveDBGate_EscapeHatchIsCommandLineOnly(t *testing.T) {
	if os.Getenv(gateHelperEnv) == "1" {
		runEscapeHatchHelper(t)
		return
	}
	for _, tc := range []struct {
		name  string
		hatch bool
		want  string
	}{
		{"hatch absent: refused", false, "GATE-RESULT: REFUSED"},
		{"hatch set at spawn: allowed", true, "GATE-RESULT: OPENED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := t.TempDir()
			if err := os.MkdirAll(filepath.Join(sandbox, ".observer"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestLiveDBGate_EscapeHatchIsCommandLineOnly$", "-test.v")
			env := stripEnv(os.Environ(), "HOME", "USERPROFILE", AllowRealDBInTestEnv, gateHelperEnv)
			env = append(env, gateHelperEnv+"=1", "HOME="+sandbox, "USERPROFILE="+sandbox)
			if tc.hatch {
				env = append(env, AllowRealDBInTestEnv+"=1")
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("child did not report %q (err=%v)\n%s", tc.want, err, out)
			}
			if err != nil {
				t.Fatalf("child test failed (err=%v)\n%s", err, out)
			}
		})
	}
}

// runEscapeHatchHelper is the child half of
// TestLiveDBGate_EscapeHatchIsCommandLineOnly. It opens the guarded path its
// spawned environment produced and reports which way the gate went.
func runEscapeHatchHelper(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("helper: UserHomeDir: %v", err)
	}
	p := filepath.Join(home, ".observer", "observer.db")
	if !pathIsGuarded(p) {
		t.Fatalf("helper: %q is not guarded — the sandbox home did not become a guarded root (roots=%v)", p, guardedRoots)
	}
	database, openErr := Open(context.Background(), Options{Path: p})
	switch {
	case openErr == nil:
		_ = database.Close()
		fmt.Println("GATE-RESULT: OPENED")
	case errors.Is(openErr, ErrRealDBInTest):
		fmt.Println("GATE-RESULT: REFUSED")
	default:
		t.Fatalf("helper: unexpected error: %v", openErr)
	}
}

// stripEnv returns env with every KEY=... entry for the named keys removed.
func stripEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// TestLiveDBGate_RootResolutionNeverFailsOpen is the regression test for codex
// FINDING 2 (HIGH). The first cut resolved the home from os.UserHomeDir alone
// and returned EMPTY on failure, so `env -u HOME go test ./...` disabled the
// whole gate while the suite stayed green — confirmed empirically. Resolution
// now consults a second, passwd-backed source and absolutises relative homes.
func TestLiveDBGate_RootResolutionNeverFailsOpen(t *testing.T) {
	fail := func() (string, error) { return "", errors.New("$HOME is not defined") }
	blank := func() (string, error) { return "   ", nil }
	passwd := func() (string, error) { return "/home/opr", nil }
	relative := func() (string, error) { return ".", nil }

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		sources []func() (string, error)
		want    []string
	}{
		{"HOME unset falls back to passwd", []func() (string, error){fail, passwd}, []string{"/home/opr/.observer"}},
		{"blank HOME falls back to passwd", []func() (string, error){blank, passwd}, []string{"/home/opr/.observer"}},
		{"relative HOME is absolutised", []func() (string, error){relative}, []string{filepath.Join(cwd, ".observer")}},
		{"duplicate sources dedupe", []func() (string, error){passwd, passwd}, []string{"/home/opr/.observer"}},
		{"every source failing yields no roots", []func() (string, error){fail, fail}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := observerRootsFrom(c.sources...)
			if len(got) != len(c.want) {
				t.Fatalf("observerRootsFrom = %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("observerRootsFrom = %v, want %v", got, c.want)
				}
			}
			for _, r := range got {
				if !filepath.IsAbs(r) {
					t.Errorf("root %q is relative — it can never match an absolute candidate, so the gate is vacuous", r)
				}
			}
		})
	}
}

// TestLiveDBGate_CapturedRootsAreTheOperatorsRealHome pins the OTHER half of
// the gate — that what it captured at init is genuinely the operator's
// ~/.observer, from whichever source resolved. Pure string comparison plus a
// stat-only predicate: safe regardless of the gate's state.
func TestLiveDBGate_CapturedRootsAreTheOperatorsRealHome(t *testing.T) {
	envHome, envErr := os.UserHomeDir()
	pwHome, pwErr := currentUserHome()
	if envErr != nil && pwErr != nil {
		t.Skipf("no home resolvable from any source (env: %v, passwd: %v)", envErr, pwErr)
	}
	// FINDING 2: a resolvable home with an empty capture means the gate is
	// silently OFF. That must be a failure, never a skip.
	if len(guardedRoots) == 0 {
		t.Fatalf("guardedRoots is EMPTY although a home resolved (env=%q err=%v, passwd=%q err=%v) — the gate is disabled",
			envHome, envErr, pwHome, pwErr)
	}
	for _, home := range []string{envHome, pwHome} {
		if strings.TrimSpace(home) == "" {
			continue
		}
		want := filepath.Join(home, ".observer")
		if !pathIsGuarded(filepath.Join(want, "observer.db")) {
			t.Errorf("the live database under %q is NOT guarded (roots=%v)", want, guardedRoots)
		}
	}
}
