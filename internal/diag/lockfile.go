package diag

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// LockInfo is the on-disk record `observer start` writes so other observer
// processes (and the doctor) can detect concurrent daemons on the same DB.
type LockInfo struct {
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
	DBPath     string    `json:"db_path"`
	BinaryPath string    `json:"binary_path"`
}

// lockGlob matches per-PID lock files written into the DB directory.
const lockGlob = "observer-*.lock"

// LockPath is the path the running process should write its lockfile to.
// One file per PID — collisions are impossible because PIDs are unique
// for the lifetime of the lock.
func LockPath(dbDir string, pid int) string {
	return filepath.Join(dbDir, fmt.Sprintf("observer-%d.lock", pid))
}

// WriteLock writes a lockfile for the running process and cleans up any
// stale locks (PIDs that no longer exist) in the same directory. Returns
// the lockfile path so the caller can RemoveLock it on shutdown.
func WriteLock(dbDir string, info LockInfo) (string, error) {
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return "", fmt.Errorf("diag.WriteLock: mkdir %s: %w", dbDir, err)
	}
	// Best-effort sweep of stale locks. Failures here are non-fatal —
	// the new lock will still write and concurrent-daemon detection
	// degrades gracefully (an extra stale entry just means an extra
	// warn line).
	cleanStaleLocks(dbDir)

	path := LockPath(dbDir, info.PID)
	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", fmt.Errorf("diag.WriteLock: marshal: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("diag.WriteLock: write %s: %w", path, err)
	}
	return path, nil
}

// RemoveLock deletes the named lockfile. Safe to call on a path that
// may already be gone.
func RemoveLock(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LiveLocks reads every observer-*.lock in dbDir, drops entries whose
// PID is no longer alive, and returns the live ones. Order is stable
// by PID for deterministic doctor output.
func LiveLocks(dbDir string) ([]LockInfo, error) {
	matches, err := filepath.Glob(filepath.Join(dbDir, lockGlob))
	if err != nil {
		return nil, err
	}
	var live []LockInfo
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var info LockInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		if info.PID == 0 {
			continue
		}
		if !processAlive(info.PID) {
			continue
		}
		live = append(live, info)
	}
	// Sort by PID (ascending) — small N, simple sort.
	for i := 1; i < len(live); i++ {
		for j := i; j > 0 && live[j-1].PID > live[j].PID; j-- {
			live[j-1], live[j] = live[j], live[j-1]
		}
	}
	return live, nil
}

// cleanStaleLocks removes lockfiles whose PIDs are no longer running.
// Errors are swallowed — this is best-effort housekeeping.
func cleanStaleLocks(dbDir string) {
	matches, _ := filepath.Glob(filepath.Join(dbDir, lockGlob))
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var info LockInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			// Garbage file — leave it; we don't want to remove a
			// file we can't even parse, in case it's something the
			// user created.
			continue
		}
		if info.PID > 0 && !processAlive(info.PID) {
			_ = os.Remove(p)
		}
	}
}

// processAlive reports whether pid corresponds to a running process.
// Cross-platform: on Unix uses Signal(0); on Windows trusts
// os.FindProcess (which actually verifies the handle, unlike its Unix
// counterpart). False negatives are possible for very-recently-exited
// processes on Windows due to handle caching, which only matters at
// startup-cleanup time and at worst leaves one stale lockfile around
// until the next `observer start`.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// On Windows, os.FindProcess opens a handle and returns an
		// error when the process doesn't exist.
		return true
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		// ESRCH: no such process. EPERM: exists but we can't signal
		// it (still alive — different uid).
		return strings.Contains(err.Error(), "permission denied")
	}
	return true
}
