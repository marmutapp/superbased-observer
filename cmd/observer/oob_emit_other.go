//go:build !unix

package main

// emitOOBLaunchHello is a no-op on non-unix platforms: the daemon's PTY
// launcher spine (and the inherited OOB FD) is unix-only (the native-Windows
// ConPTY path does not wire the trusted control channel). See oob_emit_unix.go.
func emitOOBLaunchHello() func(exitCode int) { return func(int) {} }
