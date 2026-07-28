//go:build !windows

package etw

import (
	"errors"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestStartCaptureOffWindows pins the degradation contract on the platform CI
// actually has: a clean typed refusal, a non-nil Capture whose Status explains
// why, and UNMEASURED bytes — never a fabricated zero.
func TestStartCaptureOffWindows(t *testing.T) {
	t.Parallel()

	c, err := StartCapture(Options{})
	if !errors.Is(err, ErrUnsupportedOS) {
		t.Fatalf("StartCapture() error = %v, want ErrUnsupportedOS", err)
	}
	if c == nil {
		t.Fatal("StartCapture() returned a nil Capture; a fail-open caller would lose the reason")
	}

	mode, reason := c.Status()
	if mode != processobs.NetworkAccountingUnavailable {
		t.Fatalf("Status mode = %q, want %q", mode, processobs.NetworkAccountingUnavailable)
	}
	if !strings.Contains(reason, "only available on Windows") {
		t.Fatalf("Status reason = %q, which does not explain the failure", reason)
	}

	if in, out, ok := c.NetworkBytes(1); ok || in != 0 || out != 0 {
		t.Fatalf("NetworkBytes = (%d, %d, %v), want (0, 0, false) — UNMEASURED, never a measured zero", in, out, ok)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Lifecycle calls must be inert, not panic.
	c.Forget(1)
	c.Retire(1)
}

// TestCaptureNetworkBytesContract pins the processobs.NetworkBytesFunc contract
// that Capture must satisfy — the distinction that must never be conflated:
//
//   - ok=false  → accounting is NOT LIVE. The sample is UNMEASURED.
//   - ok=true, (0,0) → accounting IS live and the process moved no bytes.
//
// Off Windows only the first half is reachable (there is no live capture here),
// so the second half is asserted against the Accumulator + status pair that
// Capture.NetworkBytes is built from. The Windows composition itself is read
// off capture_windows.go, not executed.
func TestCaptureNetworkBytesContract(t *testing.T) {
	t.Parallel()

	t.Run("a nil capture is unmeasured", func(t *testing.T) {
		t.Parallel()
		var c *Capture
		if in, out, ok := c.NetworkBytes(1); ok || in != 0 || out != 0 {
			t.Fatalf("NetworkBytes = (%d, %d, %v), want (0, 0, false)", in, out, ok)
		}
		if mode, _ := c.Status(); mode != processobs.NetworkAccountingOff {
			t.Fatalf("Status mode = %q, want %q", mode, processobs.NetworkAccountingOff)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("the zero value reports off, not unavailable", func(t *testing.T) {
		t.Parallel()
		var c Capture
		if mode, reason := c.Status(); mode != processobs.NetworkAccountingOff || reason != "" {
			t.Fatalf("Status = (%q, %q), want (%q, \"\") — nobody requested a capture", mode, reason, processobs.NetworkAccountingOff)
		}
	})

	t.Run("live and idle is a measured zero", func(t *testing.T) {
		t.Parallel()
		// The composition Capture.NetworkBytes performs: live status + an
		// accumulator that holds nothing for this pid.
		var st captureStatus
		st.set(processobs.NetworkAccountingTCP, "")
		acc := NewAccumulator(0)

		mode, _ := st.get()
		if mode != processobs.NetworkAccountingTCP {
			t.Fatalf("precondition: mode = %q", mode)
		}
		in, out, held := acc.NetworkBytes(4242)
		if held {
			t.Fatal("precondition: the accumulator should hold nothing for an unseen pid")
		}
		// Capture translates "not held, but live" into a measured zero.
		if in != 0 || out != 0 {
			t.Fatalf("accumulator returned (%d, %d) alongside ok=false", in, out)
		}
	})
}

// TestCaptureDecodeStatsContract pins the (stats, ok) contract on the platform
// CI has. The bool is load-bearing for the same reason NetworkBytes' is: Go
// cannot conditionally implement a method, so "there is no decoder to report
// on" has to be a VALUE, not an absent method.
//
// The distinction it protects is the whole reason the counter was plumbed:
// ok=false means nothing was decoded at all, and rendering that as "0 events
// were refused" would tell an operator the fixed-offset payload-length
// assumptions were exercised and held when they were never tested.
func TestCaptureDecodeStatsContract(t *testing.T) {
	t.Parallel()

	t.Run("a nil capture reports no decoder", func(t *testing.T) {
		t.Parallel()
		var c *Capture
		if s, ok := c.DecodeStats(); ok || s != (processobs.CapturerDecodeStats{}) {
			t.Fatalf("DecodeStats = (%+v, %v), want (zero, false)", s, ok)
		}
	})

	t.Run("the zero value reports no decoder", func(t *testing.T) {
		t.Parallel()
		var c Capture
		if _, ok := c.DecodeStats(); ok {
			t.Fatal("the zero value claimed a decoder exists; absence must not read as a clean zero")
		}
	})

	t.Run("a failed StartCapture reports no decoder", func(t *testing.T) {
		t.Parallel()
		c, _ := StartCapture(Options{})
		if _, ok := c.DecodeStats(); ok {
			t.Fatal("a capture that never started claimed a decoder exists")
		}
	})
}
