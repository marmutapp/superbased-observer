package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/db/migrations"
)

// This file exists for one reason: `go test -race` on this package was
// timing out at the 40-minute per-binary budget (CI run 30698650697 on
// v1.28.0 — `FAIL internal/intelligence/dashboard 2400.066s`), and the
// cost was not test logic. No single test is even 2.1% of the total.
// The dominant term is a fixed per-test setup: every test that needs a
// database calls db.Open on a fresh path, and db.Open applies all 79
// migrations (~778 statements) from scratch. Measured in isolation that
// is 3.28s/op under -race against 49ms/op for copying a pre-migrated
// file — ~66x — and this package pays it 171 times.
//
// openTestDB is a drop-in for db.Open that copies a template built once
// per test binary. runMigrations already fast-paths when the recorded
// schema version is current (internal/db/db.go:314), so the copy opens
// with no migration work and no production-code change.
//
// Analysis: docs/plans/dashboard-race-budget-2026-08-01.md.

var (
	templateOnce sync.Once
	templateDir  string
	templatePath string
	templateErr  error

	// Resolved once at package init, before any test can rewrite the
	// environment variables os.TempDir consults.
	baseTempDir = os.TempDir()
)

// openTestDB opens a test database at opts.Path, seeding it from a
// pre-migrated template when the path is new.
//
// It is deliberately behaviour-preserving relative to db.Open:
//
//   - The live-DB guard runs FIRST, so a refused path still gets no file
//     created — copying before the guard would defeat the point of it.
//   - An EXISTING file is never touched. Tests that close and reopen a
//     path, or that pre-seed a file to observe how Open handles it, must
//     see their own bytes, not the template's.
//   - Every template failure falls back to a plain db.Open. The fallback
//     is correct, only slow; TestTemplateDBMatchesMigrationSet below is
//     what keeps a silent permanent fallback from going unnoticed.
func openTestDB(ctx context.Context, opts db.Options) (*sql.DB, error) {
	database, _, err := openTestDBSeeded(ctx, opts)
	return database, err
}

// openTestDBSeeded is openTestDB plus the fact of whether this call actually
// seeded from the template.
//
// That second return exists only so a test can assert it. Without it the
// speedup is unpinnable: if the copy silently stopped happening, every test
// would still PASS — just slowly — and the suite would drift back to the
// 40-minute cliff with nothing red. Returning the fact per call, rather than
// counting copies in a package global, keeps it correct under the 70
// t.Parallel tests in this package.
func openTestDBSeeded(ctx context.Context, opts db.Options) (*sql.DB, bool, error) {
	seeded := false
	if opts.Path != "" && opts.Path != ":memory:" {
		if err := db.GuardLiveDB(opts.Path); err != nil {
			return nil, false, err
		}
		if isFreshPath(opts.Path) {
			if tmpl, terr := migratedTemplate(); terr == nil {
				if copyFileForTest(tmpl, opts.Path) == nil {
					seeded = true
				}
			}
		}
	}
	database, err := db.Open(ctx, opts)
	return database, seeded, err
}

// isFreshPath reports whether nothing at all lives at path yet.
//
// The sidecars are part of the question, not a detail. SQLite treats a `-wal`
// next to a main database as journal to REPLAY, so dropping the template over
// a stale foreign `-wal` yields that WAL's tables and loses the template's —
// including schema_meta, after which db.Open re-runs all 79 migrations on top
// of a foreign image. Without a main file SQLite instead discards the stale
// WAL, so the pre-template behaviour was correct and this is a real
// divergence. No test in this package can reach it today (every path is a
// per-test t.TempDir), but a crash-recovery or "database disappeared" test
// would, and it would be a baffling failure to debug.
func isFreshPath(path string) bool {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(candidate); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// migratedTemplate builds — once per test binary — a fully migrated
// database file and returns its path.
//
// The template is generated at runtime from the same migration set the
// code under test uses, never checked in, so it cannot drift from the
// migrations; templateGuard asserts that property rather than trusting
// it.
func migratedTemplate() (string, error) {
	templateOnce.Do(func() {
		// baseTempDir, not "" — os.MkdirTemp("") re-reads TMP/TEMP/USERPROFILE
		// at call time on Windows, and seven tests in this package t.Setenv
		// USERPROFILE to a t.TempDir(). If one of those happened to trigger
		// the build, the template would land inside a directory `testing`
		// deletes at that test's end, and every later test would silently fall
		// back to full migration with nothing red.
		dir, err := os.MkdirTemp(baseTempDir, "dashboard-db-template-")
		if err != nil {
			templateErr = err
			return
		}
		templateDir = dir
		path := filepath.Join(dir, "template.db")

		database, err := db.Open(context.Background(), db.Options{Path: path})
		if err != nil {
			templateErr = err
			return
		}
		// Fold the WAL back into the main file before closing, because a copy
		// of the main file alone is only complete if nothing is left in the
		// sidecar.
		//
		// ckptErr is NOT the guarantee here, and it would be easy to read it
		// as one: a busy checkpoint reports itself in the RESULT ROW, which
		// ExecContext discards, so this returns nil even when the checkpoint
		// did not happen. The actual guarantee is the post-close check below —
		// SQLite only deletes the sidecars on last-connection close after a
		// successful checkpoint, so "no non-empty -wal survives Close" does
		// imply the main file is complete. Keep both: the Exec makes the
		// common case cheap, the stat is what makes it correct.
		_, ckptErr := database.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
		closeErr := database.Close()
		if ckptErr != nil {
			templateErr = ckptErr
			return
		}
		if closeErr != nil {
			templateErr = closeErr
			return
		}
		if info, statErr := os.Stat(path + "-wal"); statErr == nil && info.Size() > 0 {
			templateErr = errors.New("template database left a non-empty -wal after close")
			return
		}
		templatePath = path
	})
	return templatePath, templateErr
}

func copyFileForTest(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	// Close can fail at flush (ENOSPC, EIO). Remove the destination on that
	// path too: the caller's fallback is "open normally and pay for the
	// migrations", which is only correct if there is no half-written database
	// sitting at the path for db.Open to find.
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// TestMain removes the per-binary template directory. Without it the
// template file outlives the run in the system temp dir.
func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

// TestTemplateDBMatchesMigrationSet is the staleness guard.
//
// The whole speedup rests on the template being schema-current, because
// that is what lets runMigrations take its fast path. If the template
// ever lagged the migration set, the copy would still work — db.Open
// would just migrate it the rest of the way — so the failure mode is
// silent slowness, exactly the condition this file exists to remove.
// Assert the equality instead.
func TestTemplateDBMatchesMigrationSet(t *testing.T) {
	tmpl, err := migratedTemplate()
	if err != nil {
		t.Fatalf("template unavailable (every test fell back to full migration): %v", err)
	}

	want := maxMigrationVersion(t)
	if want == 0 {
		t.Fatal("no migrations found")
	}

	path := filepath.Join(t.TempDir(), "guard.db")
	if err := copyFileForTest(tmpl, path); err != nil {
		t.Fatalf("copy template: %v", err)
	}
	// Read the copy directly. Going through db.Open would migrate it
	// first and mask the very drift being tested for.
	database, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer database.Close()

	// schema_meta is a key/value table (internal/db/db.go:321), not a
	// column-per-field one; runMigrations reads exactly this row to decide
	// whether it can take its fast path.
	var raw string
	if err := database.QueryRowContext(context.Background(),
		`SELECT value FROM schema_meta WHERE key = 'version'`).Scan(&raw); err != nil {
		t.Fatalf("read schema_meta version: %v", err)
	}
	got, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("schema_meta version %q is not an integer: %v", raw, err)
	}
	if got != want {
		t.Fatalf("template schema version = %d, migration set is at %d — the template is stale", got, want)
	}
}

// TestOpenTestDBSeedsNewPathsFromTemplate pins the property this whole file
// exists for.
//
// Every other test here would still pass if the template were never copied —
// they would just each pay 3.28s of migrations under -race again, and the
// package would drift back to the 40-minute CI cliff with nothing red. Assert
// the seeding directly, and assert that it does NOT happen for the three
// shapes that must keep the slow-but-correct path.
func TestOpenTestDBSeedsNewPathsFromTemplate(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.db")
	database, seeded, err := openTestDBSeeded(context.Background(), db.Options{Path: fresh})
	if err != nil {
		t.Fatalf("open fresh path: %v", err)
	}
	if !seeded {
		t.Fatal("a brand-new path was NOT seeded from the template — every test in this package is paying full migration cost again")
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// An existing database must be opened as-is (the invariant that makes the
	// 171-site substitution safe).
	if _, seeded, err = openTestDBSeeded(context.Background(), db.Options{Path: fresh}); err != nil {
		t.Fatalf("reopen: %v", err)
	} else if seeded {
		t.Fatal("an existing database was overwritten by the template")
	}

	// A stale sidecar with no main file is a database SQLite would recover,
	// so the template must not be dropped on top of it.
	orphan := filepath.Join(dir, "orphan.db")
	if err := os.WriteFile(orphan+"-wal", []byte("not a real wal"), 0o600); err != nil {
		t.Fatalf("write orphan wal: %v", err)
	}
	if isFreshPath(orphan) {
		t.Fatal("a path with a stale -wal was treated as fresh; the template would be copied over a journal SQLite intends to replay")
	}
}

// TestOpenTestDBPreservesExistingFile pins the invariant that makes the
// substitution safe: a path that already holds a database is opened, not
// overwritten.
func TestOpenTestDBPreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")

	database, err := openTestDB(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := database.ExecContext(context.Background(),
		`CREATE TABLE marker_only_in_first_open (x INTEGER)`); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := openTestDB(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	var n int
	if err := reopened.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'marker_only_in_first_open'`).Scan(&n); err != nil {
		t.Fatalf("query marker: %v", err)
	}
	if n != 1 {
		t.Fatal("reopening an existing path clobbered it with the template")
	}
}

func maxMigrationVersion(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	highest := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			continue
		}
		if v > highest {
			highest = v
		}
	}
	return highest
}
