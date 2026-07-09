package clinecli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// TestIsForeignMountPath pins the helper's contract: only paths
// under cross-mount-detected non-native homes match. Tests inject a
// fake AllHomes returning one native + one foreign home so the
// assertion runs identically on any host.
func TestIsForeignMountPath(t *testing.T) {
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: "/home/me", OS: crossmount.OSLinux, Origin: "native"},
			{Path: "/mnt/c/Users/marmu", OS: crossmount.OSWindows, Origin: "wsl-mnt:marmu"},
		}
	}
	cases := []struct {
		path string
		want bool
	}{
		{"/home/me/.cline/data/db/sessions.db", false},
		{"/mnt/c/Users/marmu/.cline/data/db/sessions.db", true},
		{"/mnt/c/Users/marmu/.cline/data/db/sessions.db-wal", true},
		{"/mnt/c/Users/marmu/.cline/data/db/sessions.db-shm", true},
		{"/tmp/random", false},
		{"/mnt/c/Users/other/.cline/data/db/sessions.db", false}, // wrong user
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := isForeignMountPath(tc.path); got != tc.want {
				t.Errorf("isForeignMountPath(%q) = %v; want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestStageMirrorIfForeign_NativePassThrough pins the no-op fast
// path. Native sessions.db opens directly — no copy, no cache dir
// touched. Critical: every scan goes through this helper; an
// accidental always-copy would burn I/O on every native poll.
func TestStageMirrorIfForeign_NativePassThrough(t *testing.T) {
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: "/home/me", OS: crossmount.OSLinux, Origin: "native"},
		}
	}
	got, err := stageMirrorIfForeign("/home/me/.cline/data/db/sessions.db")
	if err != nil {
		t.Fatalf("stageMirrorIfForeign: %v", err)
	}
	if got != "/home/me/.cline/data/db/sessions.db" {
		t.Errorf("native path got remapped to %q; want passthrough", got)
	}
}

// TestStageMirrorIfForeign_CopiesTrioAndReusesOnRepeat pins the
// happy path: a foreign-mount source triggers a trio copy to a
// per-source cache dir; a second call with no source mtime change
// skips the copy. Uses a fake AllHomes pointing at a tempdir so the
// test runs without depending on /mnt/c.
func TestStageMirrorIfForeign_CopiesTrioAndReusesOnRepeat(t *testing.T) {
	srcRoot := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: "/home/observer", OS: crossmount.OSLinux, Origin: "native"},
			{Path: srcRoot, OS: crossmount.OSWindows, Origin: "wsl-mnt:fake"},
		}
	}
	srcDB := filepath.Join(srcRoot, "sessions.db")
	if err := os.WriteFile(srcDB, []byte("DBv1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcDB+"-wal", []byte("WALv1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcDB+"-shm", []byte("SHMv1"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	if first == srcDB {
		t.Fatalf("foreign source returned passthrough; want mirror path")
	}
	if !strings.Contains(first, "clinecli-mirror") {
		t.Errorf("mirror path %q missing clinecli-mirror segment", first)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		got, err := os.ReadFile(first + suffix)
		if err != nil {
			t.Fatalf("read mirror sibling %s: %v", suffix, err)
		}
		want := map[string]string{"": "DBv1", "-wal": "WALv1", "-shm": "SHMv1"}[suffix]
		if string(got) != want {
			t.Errorf("mirror %s body = %q; want %q", suffix, string(got), want)
		}
	}

	// Backdate every source sibling so the mirror is clearly fresher
	// than the source — that's the condition mirrorUpToDate checks.
	// Snapshot the mirror's mtime BEFORE the second call so we can
	// detect a re-copy by observing the mtime move forward.
	past := time.Now().Add(-time.Hour)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chtimes(srcDB+suffix, past, past); err != nil {
			t.Fatal(err)
		}
	}
	beforeSecond, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	if second != first {
		t.Errorf("second mirror path = %q; want %q (stable per source)", second, first)
	}
	afterSecond, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !afterSecond.ModTime().Equal(beforeSecond.ModTime()) {
		t.Errorf("mirror mtime changed (%v -> %v); repeat call must skip the copy when source is unchanged",
			beforeSecond.ModTime(), afterSecond.ModTime())
	}
}

// TestStageMirrorIfForeign_RefreshesOnWALModification confirms the
// fast-moving-WAL detection. After bumping the WAL's mtime (the
// signal Cline emits every flush), the next mirror call must
// re-copy.
func TestStageMirrorIfForeign_RefreshesOnWALModification(t *testing.T) {
	srcRoot := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: srcRoot, OS: crossmount.OSWindows, Origin: "wsl-mnt:fake"},
		}
	}
	srcDB := filepath.Join(srcRoot, "sessions.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(srcDB+suffix, []byte("v1"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mirror, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatal(err)
	}
	// Write a sentinel into the mirror WAL.
	if err := os.WriteFile(mirror+"-wal", []byte("MIRROR-SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Bump the source WAL's mtime by 2 seconds — simulating Cline
	// flushing a turn.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(srcDB+"-wal", future, future); err != nil {
		t.Fatal(err)
	}
	// Also re-write the WAL content so the second sync isn't tripped
	// up by an mtime-only change with identical content.
	if err := os.WriteFile(srcDB+"-wal", []byte("v2-wal-after-flush"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(srcDB+"-wal", future, future); err != nil {
		t.Fatal(err)
	}

	if _, err := stageMirrorIfForeign(srcDB); err != nil {
		t.Fatalf("second mirror after WAL bump: %v", err)
	}
	got, _ := os.ReadFile(mirror + "-wal")
	if string(got) == "MIRROR-SENTINEL" {
		t.Error("mirror WAL was NOT refreshed after source WAL mtime bump; sentinel still present")
	}
	if string(got) != "v2-wal-after-flush" {
		t.Errorf("mirror WAL = %q; want refreshed copy %q", got, "v2-wal-after-flush")
	}
}

// TestStageMirrorIfForeign_HandlesMissingSiblings confirms a missing
// -wal or -shm doesn't fail the mirror call — Cline can checkpoint
// the WAL and remove it, leaving just the main .db. The mirror's
// existing stale siblings should be removed so they don't shadow
// the freshly-checkpointed source.
func TestStageMirrorIfForeign_HandlesMissingSiblings(t *testing.T) {
	srcRoot := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: srcRoot, OS: crossmount.OSWindows, Origin: "wsl-mnt:fake"},
		}
	}
	srcDB := filepath.Join(srcRoot, "sessions.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(srcDB+suffix, []byte("v1"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mirror, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate WAL checkpoint: remove source WAL + SHM, bump main
	// db's mtime so the mirror re-copies.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(srcDB + suffix)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(srcDB, []byte("v2-checkpointed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(srcDB, future, future); err != nil {
		t.Fatal(err)
	}

	if _, err := stageMirrorIfForeign(srcDB); err != nil {
		t.Fatalf("mirror after checkpoint: %v", err)
	}
	// Stale mirror WAL + SHM should be gone.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(mirror + suffix); !os.IsNotExist(err) {
			t.Errorf("mirror %s should be removed after source checkpoint; stat err=%v", suffix, err)
		}
	}
}
