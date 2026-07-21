//go:build !windows

package jobobject

import (
	"os/exec"
	"testing"
)

// TestAttachProcess_NoOpOnNonWindows pins the cross-platform stub
// behavior: non-Windows callers get a no-op Closer back, no error,
// no observable side effect on the command.
//
// The protection itself is Windows-only by design (V7-1 is a
// codex.exe-on-Windows failure mode); POSIX uses signal forwarding
// which already cascades the wrapper's death to the child.
func TestAttachProcess_NoOpOnNonWindows(t *testing.T) {
	t.Parallel()
	// /bin/true is fast and present on every POSIX. No need to call
	// Start() — the non-Windows stub doesn't inspect cmd.Process.
	cmd := exec.Command("/bin/true")

	closer, err := AttachProcess(cmd)
	if err != nil {
		t.Fatalf("non-Windows AttachProcess should never error, got: %v", err)
	}
	if closer == nil {
		t.Fatal("AttachProcess returned nil Closer")
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Closing twice is idempotent — second Close is also a no-op.
	if err := closer.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestAttachProcess_NilCmd_NoOpOnNonWindows: the non-Windows stub
// ignores cmd entirely (no validation), so nil also returns the
// no-op cleanly. This pins the contract that the stub never panics
// on degenerate inputs.
func TestAttachProcess_NilCmd_NoOpOnNonWindows(t *testing.T) {
	t.Parallel()
	closer, err := AttachProcess(nil)
	if err != nil {
		t.Fatalf("nil cmd should not error on non-Windows: %v", err)
	}
	if closer == nil {
		t.Fatal("AttachProcess returned nil Closer")
	}
	_ = closer.Close()
}
