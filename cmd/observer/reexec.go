package main

import (
	"fmt"
	"sync/atomic"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// reexec.go implements "restart the daemon from the dashboard" (docs/plans/
// dashboard-daemon-restart-plan-2026-07-14.md). The dashboard restart hook sets
// restartRequested and cancels the root context; the daemon then runs its
// NORMAL graceful shutdown (the same path SIGTERM triggers — component drain,
// launchClose PTY reap, dbCleanup), and only after every defer has run does
// main() re-exec the process image in place (execSelf). No external supervisor
// or CLI relaunch is needed, and config written by `remote enable` binds on the
// restart.
//
// Codex 2026-07-14: never shut down into a brick — preflightRestart validates
// the config would come back up BEFORE the shutdown is triggered.

// restartRequested is set by the dashboard restart hook and read by main()
// after Execute() returns (i.e. after graceful shutdown + all defers). It is
// process-global by necessity: the set happens deep in the start lifecycle, the
// check happens in main.
var restartRequested atomic.Bool

// preflightRestart validates that a restart would actually succeed before the
// daemon tears itself down. The realistic brick cause is a config edited/saved
// to an invalid state (e.g. a bad value, or `remote enable` writing a listener
// that won't validate); re-Load + re-Validate catches it, and the restart is
// REFUSED (the daemon keeps running) rather than shutting down into a process
// that won't come back. A bind conflict on a port this process does not already
// hold is a documented residual (rare; the new process fails to bind and exits,
// the same as any restart into a taken port).
func preflightRestart(configPath string) error {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return fmt.Errorf("restart refused — the current config would not load (fix it first): %w", err)
	}
	if verr := config.Validate(cfg); verr != nil {
		return fmt.Errorf("restart refused — the current config is invalid (fix it first): %w", verr)
	}
	return nil
}
