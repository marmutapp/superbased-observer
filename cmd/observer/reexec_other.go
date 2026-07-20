//go:build !unix

package main

import "errors"

// reexecSupported is false on a native-Windows daemon (no execve). The
// dashboard restart hook refuses up front with an honest message; the operator
// relaunches manually or a supervisor restarts the process. The daemon runs
// under WSL/Linux in practice, where the unix path applies.
func reexecSupported() bool { return false }

// execSelf is unsupported on this OS. main() only calls it when
// restartRequested is set, which the restart hook refuses to set unless
// reexecSupported() — so this is defence-in-depth.
func execSelf() error {
	return errors.New("in-process restart is not supported on this OS — relaunch the daemon manually")
}
