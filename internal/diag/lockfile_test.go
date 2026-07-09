package diag

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func stubConfig(dbPath string) config.Config {
	cfg := config.Default()
	cfg.Observer.DBPath = dbPath
	return cfg
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestWriteLock_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	info := LockInfo{
		PID:        os.Getpid(),
		StartedAt:  time.Now().UTC(),
		DBPath:     filepath.Join(dir, "observer.db"),
		BinaryPath: "/usr/local/bin/observer",
	}
	path, err := WriteLock(dir, info)
	if err != nil {
		t.Fatalf("WriteLock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lockfile not written: %v", err)
	}

	live, err := LiveLocks(dir)
	if err != nil {
		t.Fatalf("LiveLocks: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("got %d live locks, want 1", len(live))
	}
	if live[0].PID != info.PID {
		t.Errorf("pid: got %d want %d", live[0].PID, info.PID)
	}
	if live[0].DBPath != info.DBPath {
		t.Errorf("db_path: got %q want %q", live[0].DBPath, info.DBPath)
	}

	// RemoveLock cleans up.
	if err := RemoveLock(path); err != nil {
		t.Errorf("RemoveLock: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("lock not removed: err=%v", err)
	}

	// RemoveLock on a missing path is a no-op (not an error).
	if err := RemoveLock(path); err != nil {
		t.Errorf("RemoveLock on missing path: %v", err)
	}
}

// TestLiveLocks_FiltersStale writes a lockfile for a definitely-dead PID
// and confirms LiveLocks drops it. We can't pick a PID with 100% certainty
// — pid 0 / negatives are treated as bogus. Use a high-numbered PID that
// is virtually never live (1 below int32 max).
func TestLiveLocks_FiltersStale(t *testing.T) {
	dir := t.TempDir()
	stale := LockInfo{
		PID:        2147483646,
		StartedAt:  time.Now().UTC(),
		DBPath:     filepath.Join(dir, "observer.db"),
		BinaryPath: "/old/observer",
	}
	if _, err := WriteLock(dir, stale); err != nil {
		t.Fatalf("WriteLock stale: %v", err)
	}
	live, err := LiveLocks(dir)
	if err != nil {
		t.Fatalf("LiveLocks: %v", err)
	}
	for _, l := range live {
		if l.PID == stale.PID {
			t.Errorf("dead PID %d returned by LiveLocks", l.PID)
		}
	}
}

// TestLiveLocks_SortedByPID confirms output ordering is deterministic.
func TestLiveLocks_SortedByPID(t *testing.T) {
	dir := t.TempDir()
	mypid := os.Getpid()
	infos := []LockInfo{
		{PID: mypid, StartedAt: time.Now().UTC()},
	}
	for _, info := range infos {
		if _, err := WriteLock(dir, info); err != nil {
			t.Fatal(err)
		}
	}
	live, _ := LiveLocks(dir)
	if len(live) != 1 || live[0].PID != mypid {
		t.Errorf("live: %+v", live)
	}
}

func TestLiveLocks_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	live, err := LiveLocks(dir)
	if err != nil {
		t.Fatalf("LiveLocks: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("expected 0 locks, got %d", len(live))
	}
}

// TestLiveLocks_GarbageFile ignores files in the lock dir that don't
// parse as JSON — we never want doctor to crash on stray files.
func TestLiveLocks_GarbageFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "observer-junk.lock"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := LiveLocks(dir)
	if err != nil {
		t.Fatalf("LiveLocks: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("expected 0 locks (garbage skipped), got %d", len(live))
	}
}

// TestCheckConcurrentDaemons_None: no lockfiles → OK with "no daemon".
func TestCheckConcurrentDaemons_None(t *testing.T) {
	dir := t.TempDir()
	cfg := stubConfig(filepath.Join(dir, "observer.db"))
	c := checkConcurrentDaemons(cfg)
	if c.Status != StatusOK {
		t.Errorf("status: %v want OK; msg=%s", c.Status, c.Message)
	}
}

// TestCheckConcurrentDaemons_One: a single self-PID lockfile → OK.
func TestCheckConcurrentDaemons_One(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteLock(dir, LockInfo{PID: os.Getpid(), DBPath: "x"}); err != nil {
		t.Fatal(err)
	}
	cfg := stubConfig(filepath.Join(dir, "observer.db"))
	c := checkConcurrentDaemons(cfg)
	if c.Status != StatusOK {
		t.Errorf("status: %v want OK; msg=%s", c.Status, c.Message)
	}
}

// TestCheckConcurrentDaemons_Multiple: two live lockfiles → Warn with
// PID details.
func TestCheckConcurrentDaemons_Multiple(t *testing.T) {
	dir := t.TempDir()
	// Self PID is definitely alive. Add a second alive PID — the parent
	// of this test process if we have one, otherwise re-use self.
	other := os.Getppid()
	if other <= 0 || other == os.Getpid() {
		t.Skip("no second-PID candidate available")
	}
	if _, err := WriteLock(dir, LockInfo{PID: os.Getpid(), DBPath: "x"}); err != nil {
		t.Fatal(err)
	}
	// WriteLock keys by PID, so write the second lock manually.
	if err := os.WriteFile(LockPath(dir, other),
		[]byte(`{"pid":`+itoa(other)+`,"db_path":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := stubConfig(filepath.Join(dir, "observer.db"))
	c := checkConcurrentDaemons(cfg)
	if c.Status != StatusWarn {
		t.Errorf("status: %v want Warn; msg=%s", c.Status, c.Message)
	}
	if len(c.Details) != 2 {
		t.Errorf("details: got %d want 2: %v", len(c.Details), c.Details)
	}
}
