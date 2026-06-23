//go:build !windows

package codexipc

import (
	"context"
	"fmt"
	"syscall"
)

// terminate on POSIX sends SIGKILL to the given PID. EPERM is
// surfaced clearly so the wrapper's per-PID line directs the operator
// to re-run as the owning user (or via sudo when appropriate).
func terminate(_ context.Context, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("codexipc.terminate: invalid pid %d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("codexipc.terminate: kill PID %d: %w", pid, err)
	}
	return nil
}
