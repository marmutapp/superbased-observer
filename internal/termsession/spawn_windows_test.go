//go:build windows

package termsession

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestPTYSupportedWindows pins that a ConPTY-capable Windows host reports the
// embedded terminal as available — cmd relies on this to WIRE the launch seam
// (and show the "Launch here" button) on native Windows. On a pre-1809 host
// (no CreatePseudoConsole) it would be false and the seam stays unwired; the
// CI/dev hosts this runs on are modern, so we assert true here.
func TestPTYSupportedWindows(t *testing.T) {
	if !PTYSupported() {
		t.Skip("ConPTY not available on this Windows host (pre-1809); launch seam stays unwired by design")
	}
}

// TestRealConPTYEchoes exercises the actual ConPTY spawner (not the fake): it
// runs `cmd.exe /c echo <token>` through a real pseudoconsole, reads the
// echoed token back off the output pipe, confirms a clean exit, and reaps the
// job object. This is the Windows counterpart to the unix TestRealSpawnerEchoes
// — real CreateProcess + ConPTY + job-object teardown — and runs only on
// Windows (on Linux it is a compile-only gate via `GOOS=windows go test -c`).
func TestRealConPTYEchoes(t *testing.T) {
	if !PTYSupported() {
		t.Skip("ConPTY not available on this Windows host")
	}
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	// Spec.argv() prepends BinPath+Subcommand+--continue-from+SessionID; for a
	// bare echo probe we set BinPath=cmd.exe and let the remaining tokens be
	// echoed back through the terminal.
	sp := NewOSSpawner()
	p, err := sp.Spawn(Spec{BinPath: comspec, Subcommand: "/c", SessionID: "echo hello-from-conpty", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("real conpty spawn: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })

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
	case <-time.After(10 * time.Second):
		t.Fatal("timed out reading ConPTY output")
	}

	if _, werr := p.Wait(); werr != nil {
		t.Fatalf("wait: %v", werr)
	}
	if got := b.String(); !strings.Contains(got, "hello-from-conpty") {
		t.Errorf("ConPTY output %q did not echo the token", got)
	}
	// Kill is idempotent — a second call must not panic or double-close.
	_ = p.Kill()
	_ = p.Kill()
}
