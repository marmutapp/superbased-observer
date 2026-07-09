//go:build unix

package termsession

import (
	"strings"
	"testing"
	"time"
)

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
