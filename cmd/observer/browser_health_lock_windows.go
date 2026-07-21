//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusive takes an exclusive advisory lock on f, blocking until it
// is acquired. Windows has no flock(2); LockFileEx with
// LOCKFILE_EXCLUSIVE_LOCK is the equivalent per-handle exclusive byte-range
// lock. Locking a single byte at offset 0 is the standard whole-file-lock
// convention and is sufficient because every writer uses the same convention.
func lockFileExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

// tryLockFileExclusive attempts a NONBLOCKING exclusive advisory lock on f
// (LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY). It returns nil on
// success, errLockWouldBlock when the lock is currently held by another owner
// (ERROR_LOCK_VIOLATION, the immediate-fail signal), and the raw error for
// anything else. This is the cancellable primitive withBrowserHealthLock
// polls so it never has to abandon a blocked goroutine.
func tryLockFileExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockWouldBlock
	}
	return err
}

// unlockFile releases the advisory lock held on f.
func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
