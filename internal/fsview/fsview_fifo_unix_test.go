//go:build unix

package fsview

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadFIFO verifies a named pipe inside the project is rejected with
// ErrNotRegular rather than blocking the handler forever (finding 3). Unix-only:
// mkfifo has no portable Windows analogue, so this lives behind a build tag.
func TestReadFIFO(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	// No writer is ever opened; a blocking open would hang forever. The
	// non-blocking open + IsRegular check must return promptly with
	// ErrNotRegular.
	done := make(chan error, 1)
	go func() {
		_, err := Read(context.Background(), root, "pipe", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("Read(fifo) err = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read(fifo) blocked — non-blocking open/IsRegular guard failed")
	}
}
