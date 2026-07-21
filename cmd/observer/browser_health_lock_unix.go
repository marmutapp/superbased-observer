//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// lockFileExclusive takes an exclusive (LOCK_EX) advisory flock on f, blocking
// until it is acquired. flock associates the lock with the open file
// description, so two goroutines that each open their own fd on the same lock
// file still mutually exclude — the property the concurrency test relies on.
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// tryLockFileExclusive attempts a NONBLOCKING exclusive advisory flock on f
// (LOCK_EX|LOCK_NB). It returns nil on success, errLockWouldBlock when the
// lock is currently held by another owner (EWOULDBLOCK/EAGAIN), and the raw
// syscall error for anything else. This is the cancellable primitive
// withBrowserHealthLock polls so it never has to abandon a blocked goroutine.
func tryLockFileExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockWouldBlock
	}
	return err
}

// unlockFile releases the advisory flock held on f.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
