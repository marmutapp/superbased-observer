//go:build unix

package main

import (
	"os"
	"syscall"
)

// reexecSupported reports whether this OS can re-exec the process in place.
// Unix has execve (syscall.Exec); a native-Windows build cannot (see
// reexec_other.go) and the dashboard restart falls back to an honest message.
func reexecSupported() bool { return true }

// execSelf replaces the current process image with a fresh invocation of the
// same binary and argv — the daemon relaunches itself with no supervisor. It is
// called by main() ONLY after graceful shutdown + every defer has run (DB
// closed, PTY children reaped, listeners released), so the new image binds
// cleanly. os.Executable() is the running binary (not a wrapper/shim); os.Args
// + os.Environ() reconstruct the same `observer start` invocation. On success
// it never returns (the image is replaced); it returns only on error.
func execSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
