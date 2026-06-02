//go:build !windows

package jobobject

import (
	"io"
	"os/exec"
)

// AttachProcess is a no-op on non-Windows builds. The V7-1 zombie
// failure mode is Windows-specific (the codex.exe Stop-Process path
// at v1.7.5); POSIX uses signal forwarding which already cascades.
// Returns a no-op Closer so call sites can defer Close() uniformly.
func AttachProcess(_ *exec.Cmd) (io.Closer, error) {
	return noopCloser{}, nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
