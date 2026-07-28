//go:build unix

package termsession

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRealSpawnerReportsLivePID pins the process-attribution seam against the
// REAL spawner: the pid the Manager publishes must be a genuinely live OS
// process (and, on Linux, its own process-group leader — Setsid — so the whole
// `observer <tool>` → `<tool>` subtree hangs off it). This is what makes the
// pid a sound direct-attribution seed rather than a number.
func TestRealSpawnerReportsLivePID(t *testing.T) {
	sp := NewOSSpawner()
	p, err := sp.Spawn(Spec{BinPath: "/bin/sleep", Subcommand: "30"})
	if err != nil {
		t.Fatalf("real spawn: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	rep, ok := p.(ProcessReporter)
	if !ok {
		t.Fatal("the unix PTY does not implement ProcessReporter — the process-attribution seam is unwired")
	}
	pid := rep.Pid()
	if pid <= 0 {
		t.Fatalf("Pid() = %d, want a live pid", pid)
	}
	// Signal 0 probes liveness without disturbing the process.
	if serr := syscall.Kill(pid, 0); serr != nil {
		t.Fatalf("pid %d is not live: %v", pid, serr)
	}
	// Setsid makes the child its own process-group leader, so the whole
	// subtree is reachable from this one pid.
	if pgid, gerr := syscall.Getpgid(pid); gerr != nil || pgid != pid {
		t.Fatalf("Getpgid(%d) = (%d,%v), want the pid itself (process-group leader)", pid, pgid, gerr)
	}
	if runtime.GOOS == "linux" {
		if _, serr := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); serr != nil {
			t.Fatalf("/proc/%d missing — the reported pid is not a real process: %v", pid, serr)
		}
	}
}

// TestPTYSupportedUnix pins that the unix build reports the embedded
// terminal as available — cmd relies on this to WIRE the launch seam (and
// thus show the "Launch here" affordance) on unix, while the windows build's
// ptySupported()==false leaves it unwired. The windows side is covered by the
// GOOS=windows compile + the launch.go error mapping test.
func TestPTYSupportedUnix(t *testing.T) {
	if !PTYSupported() {
		t.Fatal("PTYSupported() = false on unix, want true (embedded terminal must be available)")
	}
}

// TestRealSpawnerEchoes exercises the actual creack/pty spawner (not the
// fake): it starts /bin/echo through a real PTY, reads the echoed argv back
// off the master fd, confirms a clean exit, and reaps the process group.
// This is the integration coverage the fake-PTY manager tests can't give —
// real fork+exec+ioctl+Setsid. The argv shape is the server-derived [bin,
// subcommand, --continue-from, id], so echo prints all of it; we assert the
// subcommand token survived the round-trip through the terminal.
func TestRealSpawnerEchoes(t *testing.T) {
	sp := NewOSSpawner()
	p, err := sp.Spawn(Spec{BinPath: "/bin/echo", Subcommand: "hello-from-pty", SessionID: "sess-x"})
	if err != nil {
		t.Fatalf("real spawn: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

	// Read until the child exits (a pty master returns EIO on Linux when the
	// slave side closes, so break on any error, not just io.EOF).
	var b strings.Builder
	buf := make([]byte, 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			n, rerr := p.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading PTY output")
	}

	code, werr := p.Wait()
	if werr != nil {
		t.Fatalf("wait: %v", werr)
	}
	if code != 0 {
		t.Errorf("echo exit code = %d, want 0", code)
	}
	if got := b.String(); !strings.Contains(got, "hello-from-pty") {
		t.Errorf("PTY output %q did not echo the subcommand token", got)
	}
}
