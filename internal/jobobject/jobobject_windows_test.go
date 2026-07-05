//go:build windows

package jobobject

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestAttachProcess_NilCmd: nil cmd surfaces a clear error rather
// than panicking.
func TestAttachProcess_NilCmd(t *testing.T) {
	t.Parallel()
	if _, err := AttachProcess(nil); err == nil {
		t.Errorf("expected error for nil cmd")
	}
}

// TestAttachProcess_NotStarted: cmd that hasn't been Start()'d yet
// (Process is nil) surfaces a clear error rather than panicking.
func TestAttachProcess_NotStarted(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("cmd.exe", "/c", "echo hi")
	if _, err := AttachProcess(cmd); err == nil {
		t.Errorf("expected error for un-started cmd")
	}
}

// TestAttachProcess_KillOnClose pins the load-bearing V7-1
// behavior: closing the Job Object handle terminates the assigned
// child process via KILL_ON_JOB_CLOSE.
//
// The child is a "ping localhost" loop — long-running enough that
// we can verify it's running, then verify it terminates after
// Close. We assert via cmd.Wait()'s exit error, not via cmd.Process
// state (which doesn't reflect a Windows-side terminate until the
// process tabe is reaped).
func TestAttachProcess_KillOnClose(t *testing.T) {
	t.Parallel()

	// "ping -n 30 127.0.0.1" runs ~30s; if Job Object close doesn't
	// terminate it, Wait blocks past the test timeout. timeout via
	// t.Deadline in case the kill doesn't fire.
	cmd := exec.Command("ping.exe", "-n", "30", "127.0.0.1")
	// Hide the console window so test runs don't pop a CMD flash.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	closer, err := AttachProcess(cmd)
	if err != nil {
		// Best-effort cleanup since Start succeeded.
		_ = cmd.Process.Kill()
		t.Fatalf("AttachProcess: %v", err)
	}

	// Give the child a beat to settle into its loop.
	time.Sleep(200 * time.Millisecond)

	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close, Windows should terminate the child via
	// KILL_ON_JOB_CLOSE. Wait should return within a short window
	// (vs. the ~30s ping would otherwise run).
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-done:
		// Either exit error or nil — both confirm the process is
		// reaped. KILL_ON_JOB_CLOSE usually surfaces as a non-zero
		// exit; the value isn't load-bearing for our pin.
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("child did not die after Job Object close — KILL_ON_JOB_CLOSE may not be firing")
	}
}

// TestJobHandle_DoubleCloseSafe: closing the same handle twice is
// idempotent. Important because deferred Close in the call site
// might race with an explicit earlier Close.
func TestJobHandle_DoubleCloseSafe(t *testing.T) {
	t.Parallel()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	h := &jobHandle{handle: job}
	if err := h.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
