package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// AllowRealDBInTestEnv names the ONE escape hatch for [ErrRealDBInTest].
//
// It exists because a human operator occasionally has a legitimate reason to
// point a `go test` run at the live data directory — reproducing a migration
// failure against the real corpus, or driving a one-off forensic query through
// an existing test harness rather than writing a throwaway `main`. That is a
// deliberate, typed-on-the-command-line act:
//
//	OBSERVER_ALLOW_REAL_DB_IN_TEST=1 go test ./internal/db/...
//
// Setting it from inside Go code does NOT work, by construction: the value is
// read ONCE at package-init time (see [allowRealDB]), and a test binary
// receives its environment before any TestMain or t.Setenv runs. An earlier
// cut read it per-call, which meant `t.Setenv(AllowRealDBInTestEnv, "1")`
// silently re-opened the exact hole this gate closes.
const AllowRealDBInTestEnv = "OBSERVER_ALLOW_REAL_DB_IN_TEST"

// ErrRealDBInTest is the sentinel [GuardLiveDB] (and therefore [Open]) returns
// when a test binary asks for a database inside the operator's REAL
// ~/.observer directory.
//
// Incident (2026-07-30, "task #17"): a working-tree test resolved
// config.Default()'s `~/.observer/observer.db`, opened it, and ran agent
// migration 078 against the operator's live 55k-row production database. The
// schema change happened to be correct, so nothing was lost — but the class is
// severe and unbounded: [Open] applies EVERY pending migration, so any test
// that reaches the real path can rewrite the operator's data with whatever the
// working tree happens to contain.
//
// This is the same class as the 2026-07-31 hook-registrar incident that wrote
// test hook entries into the operator's real settings.json, and it is closed
// the same structural way: a runtime gate at the seam, not a convention.
var ErrRealDBInTest = errors.New("refusing to open the live SuperBased database from a test")

// guardedRoots holds every spelling of the operator's REAL ~/.observer
// directory that the gate refuses. It is EMPTY in a production build, which is
// what makes [GuardLiveDB] free there.
//
// Two properties are deliberate:
//
// Captured at package-init time. A test that fakes its home
// (t.Setenv("HOME", t.TempDir()), the correct and common pattern) must NOT be
// able to move the gate's notion of "real": if the value were resolved lazily
// at Open time, a test could point HOME at a tempdir and then hand [Open] the
// hard-coded real path, and the gate would compare it against the fake home
// and wave it through. Package init runs before any test function.
//
// Resolved from MULTIPLE sources, not just $HOME. An earlier cut used
// os.UserHomeDir alone and returned empty when it failed — so `env -u HOME go
// test ./...` silently disabled the entire gate while the suite stayed green.
// os/user's passwd-backed lookup is consulted as well, and every root is made
// absolute, so an unset or relative $HOME can no longer switch the gate off.
//
// KNOWN RESIDUAL (deliberate, documented rather than closed): a test that
// runs under an already-sandboxed HOME *and* hard-codes the operator's
// absolute path as a string literal is still gated by the os/user root on a
// normal machine, but on a host where os/user also reports the sandbox there
// is nothing left to compare against. That shape is outside the incident
// class: the incident was a CONFIG-RESOLVED path (config.Default() →
// expandHome → $HOME), and a pre-sandboxed HOME sandboxes config resolution
// correctly. Closing the string-literal case would require pinning a path the
// process has no independent way to learn.
var guardedRoots = captureGuardedRoots()

// allowRealDB records whether the operator opened [AllowRealDBInTestEnv] on
// the command line that spawned this test binary. Captured once at init so
// test code cannot set it — see the note on [AllowRealDBInTestEnv].
var allowRealDB = captureAllowRealDB()

// liveDBBasenames are the files whose INODES the gate additionally refuses,
// so a hard link (which has no symlink target to resolve and no path prefix
// to match) cannot alias the live database from outside the directory.
var liveDBBasenames = [...]string{"observer.db", "observer.db-wal", "observer.db-shm"}

// captureAllowRealDB reads the escape-hatch env var, but ONLY inside a test
// binary. Production does no env read at all.
func captureAllowRealDB() bool {
	if !testing.Testing() {
		return false
	}
	return strings.TrimSpace(os.Getenv(AllowRealDBInTestEnv)) != ""
}

// captureGuardedRoots resolves the operator's real ~/.observer directory from
// every source available, but ONLY inside a test binary.
//
// Production cost is one string comparison: testing.Testing() reads a
// package-scope string in the testing package that cmd/link sets to "1" via
// -X for test binaries. It is deliberately NOT a compile-time constant (the
// Go source comments on testBinary say the compiler must not fold it), so the
// honest claim is "one string compare at init and one len(guardedRoots) check
// per guarded call", not "compiled away". No filesystem access, no env read,
// no os/user lookup and no allocation happens on the production path.
func captureGuardedRoots() []string {
	if !testing.Testing() {
		return nil
	}
	return observerRootsFrom(os.UserHomeDir, currentUserHome)
}

// currentUserHome resolves the home directory from the OS user database
// (passwd / SAM), which is independent of $HOME. It is the fallback that keeps
// `env -u HOME go test ./...` from disabling the gate.
func currentUserHome() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("db.currentUserHome: %w", err)
	}
	return u.HomeDir, nil
}

// observerRootsFrom builds the deduplicated, ABSOLUTE set of ~/.observer
// directories to guard, given one or more home-directory sources. Sources that
// error or yield a blank home are skipped, never fatal; a relative home (the
// `HOME=.` shape) is made absolute so it can still match absolute candidates
// rather than silently matching nothing. Each root's symlink-resolved spelling
// is added alongside the literal one.
//
// Split out from [captureGuardedRoots] so the fail-open cases can be unit
// tested without re-running package init.
func observerRootsFrom(sources ...func() (string, error)) []string {
	var roots []string
	seen := make(map[string]bool)
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return
			}
			p = abs
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			roots = append(roots, p)
		}
	}
	for _, src := range sources {
		home, err := src()
		if err != nil || strings.TrimSpace(home) == "" {
			continue
		}
		dir := filepath.Join(home, ".observer")
		add(dir)
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			add(resolved)
		}
	}
	return roots
}

// GuardLiveDB refuses a database path that lands inside the operator's real
// ~/.observer directory while running under `go test`, and returns nil
// otherwise (always, in a production build).
//
// It is exported so the ONE implementation can be shared by every SQLite
// opener that could reach the live file — internal/db's [Open] and
// runMigrations, and internal/orgserver/db's equivalents. Keeping one owner
// matters more than the import: a second copy of this predicate would drift.
//
// The gate is directory-scoped rather than pinned to `observer.db` alone: the
// live database drags `-wal` / `-shm` siblings, `observer db import` stages
// files next to it, and no legitimate test has business creating a SQLite file
// in the operator's live data directory under any name. ":memory:" is always
// allowed, and so is anything outside that directory — which is every
// t.TempDir()-based test in the tree.
//
// ACCEPTED LIMITATION: a sandbox physically located under ~/.observer (say
// `TMPDIR=~/.observer/tmp`) is refused too, so t.TempDir()-based tests would
// break under that setting; that is deliberate policy — the gate cannot
// distinguish a sanctioned sandbox from the live directory by path alone, and
// refusing is the safe direction.
//
// It returns an error rather than panicking: [Open] is library code, its
// callers already handle an Open error, and an error carries the remedy text
// to the developer who tripped it. (The hook-side sandbox gates skip rather
// than panic for the same reason.)
func GuardLiveDB(path string) error {
	// Production build, or no home we could resolve from any source: nothing
	// to guard. This is the only work the non-test path ever does.
	if len(guardedRoots) == 0 {
		return nil
	}
	if path == ":memory:" {
		return nil
	}
	if !pathIsGuarded(path) {
		return nil
	}
	if allowRealDB {
		return nil
	}
	return fmt.Errorf("db.GuardLiveDB: %w: %q resolves into the live data directory %q.\n"+
		"  A test must open its database under t.TempDir() (or \":memory:\"). "+
		"Opening the real path runs EVERY pending migration against the operator's production data — "+
		"that is incident \"task #17\" (2026-07-30).\n"+
		"  If you genuinely mean it, re-run with %s=1 on the command line (it is read at process start, never from code).",
		ErrRealDBInTest, path, guardedRoots[0], AllowRealDBInTestEnv)
}

// GuardLiveDBHandle applies [GuardLiveDB] to the file an ALREADY-OPEN handle
// is attached to, resolved via `PRAGMA database_list`.
//
// It exists because opening the file is not the damage — running migrations
// is, and a caller can reach the migration runner without going through
// [Open] (internal/db/migrations_test.go opens raw handles with database/sql
// and calls runMigrations directly). Guarding only the path seam would leave
// that route open, so the runners guard the handle as well.
//
// It fails OPEN if the pragma cannot be read: the path-form gate is the
// primary net, and a handle whose filename cannot be determined is not
// evidence of the live database.
func GuardLiveDBHandle(ctx context.Context, database *sql.DB) error {
	if len(guardedRoots) == 0 || database == nil {
		return nil
	}
	rows, err := database.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return nil //nolint:nilerr // fail open: see doc comment.
	}
	defer func() { _ = rows.Close() }()
	var mainFile string
	for rows.Next() {
		var seq int
		var name, file sql.NullString
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return nil //nolint:nilerr // fail open: see doc comment.
		}
		if name.Valid && name.String == "main" && file.Valid {
			mainFile = file.String
		}
	}
	if err := rows.Err(); err != nil || strings.TrimSpace(mainFile) == "" {
		return nil
	}
	return GuardLiveDB(mainFile)
}

// pathIsGuarded reports whether path reaches any guarded root, by prefix, by
// symlink, or by inode.
//
// Three checks, because each closes a different bypass:
//
//  1. Literal prefix on the cleaned absolute path — the incident itself.
//  2. Symlink resolution. The FULL path is resolved when it already exists,
//     which is what catches a symlink in the FINAL component
//     (`/tmp/live.db -> ~/.observer/observer.db`); an earlier cut resolved
//     only the parent directory and re-attached the original basename, so
//     that spelling walked straight through the gate into runMigrations.
//     When the final component does not exist yet (the common create-a-new-DB
//     case) the parent is resolved instead.
//  3. os.SameFile against the live database and its -wal/-shm siblings — a
//     HARD link has no target to resolve and shares no path prefix, so
//     nothing above can see it. Bounded to three stats, and only reached for
//     a candidate that already exists.
func pathIsGuarded(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}

	cands := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		cands = append(cands, resolved)
	} else if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		cands = append(cands, filepath.Join(resolvedDir, filepath.Base(abs)))
	}
	for _, c := range cands {
		for _, root := range guardedRoots {
			if c == root || strings.HasPrefix(c, root+string(filepath.Separator)) {
				return true
			}
		}
	}
	return aliasesLiveDB(abs)
}

// aliasesLiveDB reports whether abs is the SAME FILE (same device+inode) as
// the live database or one of its WAL/SHM siblings under any guarded root.
// This is the hard-link case; see check 3 in [pathIsGuarded].
func aliasesLiveDB(abs string) bool {
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return false
	}
	for _, root := range guardedRoots {
		for _, base := range liveDBBasenames {
			live, err := os.Stat(filepath.Join(root, base))
			if err != nil {
				continue
			}
			if os.SameFile(info, live) {
				return true
			}
		}
	}
	return false
}
