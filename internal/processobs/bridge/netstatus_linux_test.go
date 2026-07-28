//go:build linux

package bridge

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// helloOnlyStream is a capturer that announces a LIVE network capture and then
// produces nothing — the shape that drives the respawn loop to its failure cap.
func helloOnlyStream(t *testing.T, h Hello) string {
	t.Helper()
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Hello(h); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestLoopWithdrawsNetworkClaimOnGiveUp drives the full respawn→give-up path
// with a capturer that claims live TCP accounting and then dies.
//
// It pins the anti-stale rule end to end: once the capturer is gone nothing is
// counting bytes, so the "tcp" it announced must NOT survive it, and the
// terminal state must name the cause rather than leaving the daemon's generic
// default in place.
func TestLoopWithdrawsNetworkClaimOnGiveUp(t *testing.T) {
	status := &processobs.NetworkAccounting{}
	body := helloOnlyStream(t, Hello{
		Backend:               "poll+etw",
		OS:                    "windows",
		PID:                   1,
		NetworkAccountingMode: processobs.NetworkAccountingTCP,
	})
	b := &Backend{
		resolvedPath: writeFakeCapturer(t, body, 0),
		out:          make(chan processobs.RawEvent, 4),
		stop:         make(chan struct{}),
		minBackoff:   time.Millisecond,
		maxBackoff:   2 * time.Millisecond,
		netStatus:    status,
	}

	done := make(chan struct{})
	go func() { b.loop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not give up within timeout")
	}

	mode, reason := status.Status()
	if mode != processobs.NetworkAccountingUnavailable {
		t.Fatalf("mode = %q, want %q — a dead capturer must not keep claiming live accounting",
			mode, processobs.NetworkAccountingUnavailable)
	}
	if !strings.Contains(reason, "capture disabled") {
		t.Fatalf("reason = %q, want it to name the give-up", reason)
	}
}

// TestRunCapturerAdoptsHelloNetworkStatus proves the status really does travel
// on the wire through the real spawn+decode path, not just through the unit
// seam.
func TestRunCapturerAdoptsHelloNetworkStatus(t *testing.T) {
	status := &processobs.NetworkAccounting{}
	body := helloOnlyStream(t, Hello{
		Backend:                 "poll",
		OS:                      "windows",
		PID:                     1,
		NetworkAccountingMode:   processobs.NetworkAccountingUnavailable,
		NetworkAccountingReason: "ETW network capture could not start: not elevated",
	})
	b := &Backend{
		resolvedPath: writeFakeCapturer(t, body, 0),
		out:          make(chan processobs.RawEvent, 4),
		stop:         make(chan struct{}),
		netStatus:    status,
	}
	if _, err := b.runCapturer(context.Background()); err != nil {
		t.Fatalf("runCapturer: %v", err)
	}
	mode, reason := status.Status()
	if mode != processobs.NetworkAccountingUnavailable || !strings.Contains(reason, "not elevated") {
		t.Fatalf("status = (%q, %q), want the capturer's own unavailable/not-elevated", mode, reason)
	}
}
