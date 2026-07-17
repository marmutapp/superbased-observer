//go:build unix

package main

import (
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termoob"
)

// oob_emit_unix.go is the TRUSTED out-of-band launcher-emission half of F3
// (plan §2.1b / F3 channel (a)). When the observer daemon spawns an
// `observer <tool>` launcher it hands it an inherited pipe write end (fd 3) and
// the OBSERVER_OOB_* env. This process — running as that launcher — authenticates
// the channel with a framed Hello (carrying the run's correlation nonce so the
// daemon can correlate the run to the session it produces at oob confidence) and
// emits lifecycle transitions the daemon cannot forge from the untrusted PTY
// stream. The FD is set close-on-exec so the untrusted tool child never inherits
// it (§2.1b — the child must not be able to write to the trusted channel).
//
// It is best-effort and fail-open: any error leaves the launcher running
// normally (the daemon still observes exit via termsession.Wait).

// emitOOBLaunchHello opens the inherited OOB FD (when present), authenticates
// the channel, emits Hello + launcher_started, and returns a finalizer that
// emits tool_exec_end with the process exit code and closes the FD. When no OOB
// env is present (a normal, non-daemon-spawned invocation) it returns a no-op.
func emitOOBLaunchHello() func(exitCode int) {
	noop := func(int) {}
	fdStr := os.Getenv(envOOBFD)
	auth := os.Getenv(envOOBAuth)
	if fdStr == "" || auth == "" {
		return noop
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil || fd < 3 {
		return noop
	}
	// Prevent the untrusted tool child from inheriting the trusted channel.
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "observer-oob")
	if f == nil {
		return noop
	}
	enc := termoob.NewEncoder(f)
	if err := enc.WriteHello(termoob.Hello{
		AuthToken:        auth,
		CorrelationToken: os.Getenv(envOOBCorr),
		Tool:             os.Getenv(envOOBTool),
		PID:              os.Getpid(),
	}); err != nil {
		_ = f.Close()
		return noop
	}
	_ = enc.WriteLifecycle(termoob.Lifecycle{Phase: termoob.PhaseLauncherStarted, At: time.Now().UnixNano()})
	return func(exitCode int) {
		_ = enc.WriteLifecycle(termoob.Lifecycle{
			Phase:    termoob.PhaseToolExecEnd,
			ExitCode: &exitCode,
			At:       time.Now().UnixNano(),
		})
		_ = f.Close()
	}
}
