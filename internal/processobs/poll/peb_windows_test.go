//go:build windows

package poll

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestReadProcessParametersChild verifies the PEB read recovers the command
// line + cwd of a same-user spawned child — the fields ToolHelp omits. Runs
// natively on the Windows host.
func TestReadProcessParametersChild(t *testing.T) {
	tmp := t.TempDir()
	// Spawn ping DIRECTLY (not via `cmd /c`): cmd would fork ping as a
	// grandchild that inherits the cwd and outlives the cmd.exe we kill,
	// leaving the temp dir locked for t.TempDir's cleanup. ping itself is the
	// long-lived process holding the cwd, so killing it releases the dir.
	child := exec.Command("ping", "-n", "30", "127.0.0.1")
	child.Dir = tmp
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// Kill AND reap before the test ends: while the child lives with its cwd
	// set to the temp dir, Windows refuses to delete that dir (t.TempDir's
	// cleanup would fail). Wait() blocks until the handle is released.
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	time.Sleep(300 * time.Millisecond) // let the child's PEB settle

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, uint32(child.Process.Pid))
	if err != nil {
		t.Fatalf("open child: %v", err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	argv, cwd, ok := readProcessParameters(h)
	if !ok {
		t.Fatal("readProcessParameters returned ok=false for a same-user child")
	}
	if !strings.Contains(strings.ToLower(strings.Join(argv, " ")), "ping") {
		t.Errorf("argv missing the command: %q", argv)
	}
	if !strings.EqualFold(strings.TrimRight(cwd, `\`), strings.TrimRight(tmp, `\`)) {
		t.Errorf("cwd = %q, want %q", cwd, tmp)
	}
}

// TestPlatformEnumerateEnrichesSelf confirms the Windows enumerate now fills
// argv + cwd (via the PEB read) for at least our own process.
func TestPlatformEnumerateEnrichesSelf(t *testing.T) {
	procs, err := platformEnumerate()
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	var self *ProcInfo
	for i := range procs {
		if procs[i].PID == os.Getpid() {
			self = &procs[i]
			break
		}
	}
	if self == nil {
		t.Fatal("did not find self in the process table")
	}
	if len(self.Argv) == 0 {
		t.Error("self has no argv — PEB enrichment failed")
	}
	if self.CWD == "" {
		t.Error("self has no cwd — PEB enrichment failed")
	}
}
