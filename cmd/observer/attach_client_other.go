//go:build !unix

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// runAttachSession is the non-unix stub for `observer <tool> --attach`. The
// interactive attach client needs POSIX raw-mode termios, job-control signals
// (SIGTSTP/SIGCONT/SIGWINCH), and an AF_UNIX control socket that the daemon can
// gate owner-only via directory permissions; native Windows would need a named
// pipe + a different console model. Per the session-attach design (§6 decision
// 3) that is a deliberate v1 scope cut: Linux/WSL-first. Return an honest error
// instead of silently doing nothing, and keep the build green everywhere (B2-1).
func runAttachSession(_ context.Context, in attachLaunch) error {
	// Name the LAUNCHER subcommand (e.g. `observer claude`), not the registry
	// tool name (`claude-code`), so the copy matches the command the operator
	// actually typed (B3-5). Fall back to the tool name if unresolved.
	label := in.tool
	if capab, ok := integration.For(in.tool); ok && capab.Attach != nil && capab.Attach.Subcommand != "" {
		label = capab.Attach.Subcommand
	}
	msg := fmt.Sprintf(
		"observer %s --attach is Linux/WSL-only in v1; native Windows support is a designed follow-up. Launch without --attach, or run the launcher inside WSL.",
		label,
	)
	if in.stderr != nil {
		fmt.Fprintln(in.stderr, msg)
	}
	return errors.New(msg)
}
