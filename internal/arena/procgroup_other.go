//go:build !unix

package arena

import "os/exec"

// setProcGroup is a no-op off unix: there is no portable Setpgid, so the
// child runs in the parent's process group. Arena's headless drives are a
// unix/WSL workflow; this keeps the package building for windows without a
// behavior change on the platforms arena actually runs on.
func setProcGroup(cmd *exec.Cmd) {
	_ = cmd
}

// killProcGroup best-effort kills just cmd's own process off unix — there
// is no portable process-group kill, so a hung grandchild may outlive the
// drive (acceptable: arena is not a windows workflow, and this beats a
// build break).
func killProcGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
