//go:build unix

package dblease

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// available is true wherever syscall.Flock is real (every unix target
// this binary ships for: linux, darwin, freebsd, ...). Mirrors the
// unix/other split in cmd/observer/prune_diskcheck_{unix,other}.go and
// internal/orgserver/policykey_open_{unix,other}.go.
const available = true

// tryAcquireFile opens (creating if needed) the lock file at path and
// takes a non-blocking exclusive flock on it. syscall.EWOULDBLOCK means
// another process already holds it — that is a normal "not acquired"
// result, not an error. Any other failure is a real error the caller
// must fail open on.
func tryAcquireFile(path string) (release func(), acquired bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) //nolint:gosec // G304: path is built from the operator's own DB dir + a fixed lease name, never request data.
	if err != nil {
		return noop, false, fmt.Errorf("dblease: open lock file %s: %w", path, err)
	}
	if lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lerr != nil {
		_ = f.Close()
		if errors.Is(lerr, syscall.EWOULDBLOCK) {
			return noop, false, nil
		}
		return noop, false, fmt.Errorf("dblease: flock %s: %w", path, lerr)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return release, true, nil
}
