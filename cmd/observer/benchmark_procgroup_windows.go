//go:build windows

package main

import "os/exec"

// setProcGroup is a no-op on Windows for v1: the benchmark runner is
// operator-invoked on Linux/WSL (the Phase-0 spike environment). Codex's
// Windows Job Object kill-on-close (cmd/observer/codex.go) already bounds a
// launched codex tree; a dedicated benchmark Windows job is a follow-up if the
// rig is ever run natively on Windows.
func setProcGroup(*exec.Cmd) {}

// killProcGroup falls back to killing just the direct child; exec.CommandContext
// already does this on ctx cancellation.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
