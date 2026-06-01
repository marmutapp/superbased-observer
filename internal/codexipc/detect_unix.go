//go:build !windows

package codexipc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// detect on POSIX uses a two-stage scan:
//
//  1. Fast path — stat ${CODEX_HOME:-~/.codex}/app-server-control.sock.
//     If absent, no holder is bound; return empty without spending
//     the ~10 ms it takes to fork `ps`.
//  2. Slow path — run `ps -A -o pid,comm,args`, filter to processes
//     whose comm contains "codex" and args contains "app-server" but
//     not "--type=". Hand the bytes to parsePSOutput.
//
// macOS and Linux share the `-A -o pid,comm,args` ps spec, so one
// implementation covers both.
func detect(ctx context.Context) ([]Process, error) {
	if !sockPresent() {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid,comm,args")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codexipc.detect: ps: %w", err)
	}
	return parsePSOutput(out), nil
}

func sockPresent() bool {
	for _, path := range candidateSockPaths() {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func candidateSockPaths() []string {
	var paths []string
	if home := os.Getenv("CODEX_HOME"); home != "" {
		paths = append(paths, filepath.Join(home, "app-server-control.sock"))
	}
	if userHome, err := os.UserHomeDir(); err == nil && userHome != "" {
		paths = append(paths, filepath.Join(userHome, ".codex", "app-server-control.sock"))
	}
	return paths
}
