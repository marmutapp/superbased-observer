//go:build windows

package codexipc

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// terminate on Windows shells out to `taskkill /F /PID <pid>`.
// Surfaces stderr verbatim on failure so the operator sees the
// underlying reason (typically "Access is denied" when the codex.exe
// belongs to an elevated session and observer is running unelevated;
// the wrapper's per-PID warning line then directs the operator to
// re-run elevated or terminate via Task Manager).
func terminate(ctx context.Context, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("codexipc.terminate: invalid pid %d", pid)
	}
	cmd := exec.CommandContext(ctx, "taskkill.exe", "/F", "/PID", strconv.Itoa(pid)) //nolint:gosec // G204: fixed taskkill invocation; the only variable is a validated integer PID.
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("codexipc.terminate: taskkill PID %d: %w: %s", pid, err, trimmed)
		}
		return fmt.Errorf("codexipc.terminate: taskkill PID %d: %w", pid, err)
	}
	return nil
}
