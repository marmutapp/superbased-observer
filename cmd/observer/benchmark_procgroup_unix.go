//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcGroup launches the harness in its own process group so the runner can
// escalate a signal to the whole group (plan §3.2 step 2) — a hung grandchild
// (codex app-server) can't outlive the cell.
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup sends SIGKILL to the harness's process group. Called from
// cmd.Cancel on context cancellation/timeout.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid targets the process group (Setpgid made pgid == pid).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
