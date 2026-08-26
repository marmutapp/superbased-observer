//go:build unix

package arena

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts cmd's child into its own process group (Setpgid) so the
// whole tree can be signalled together on a timeout. Serialized by
// procGroupMu because setpgid races if two children in the same group start
// simultaneously. Unix-only; see procgroup_other.go for the fallback.
func setProcGroup(cmd *exec.Cmd) {
	procGroupMu.Lock()
	defer procGroupMu.Unlock()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup SIGKILLs the process group led by cmd's process so hung
// grandchildren cannot outlive a timed-out drive. Unix-only.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
