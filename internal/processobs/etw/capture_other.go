//go:build !windows

package etw

import (
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// Capture is the non-Windows placeholder for a running ETW network capture. It
// can never observe anything: StartCapture always fails with ErrUnsupportedOS.
//
// It exists so OS-agnostic wiring — the process bridge, the backend selector —
// compiles unchanged on linux and darwin, the same shape
// linuxebpf/backend_other.go and session_other.go already use. Every method
// mirrors the Windows one, including being safe on the zero value and on a nil
// receiver.
//
// It carries the status field and NOT the accumulator or the session: there is
// nothing off Windows to accumulate, and an always-empty accumulator would only
// invite someone to read a zero out of it. The status IS carried, because the
// honest answer here — "unavailable, because this is not Windows" — is
// information, and reporting "off" instead would read as "nobody asked for it".
type Capture struct {
	status captureStatus
}

// StartCapture always fails off Windows.
//
// It returns a NON-NIL *Capture alongside the error for the same reason the
// Windows implementation does: the returned value reports
// NetworkAccountingUnavailable with a legible reason, so a fail-open caller
// that keeps it still tells the operator why there are no bytes rather than
// showing silence. Every method is nil-safe, so discarding it is equally fine.
func StartCapture(Options) (*Capture, error) {
	c := &Capture{}
	c.status.set(processobs.NetworkAccountingUnavailable, captureUnavailableReason(ErrUnsupportedOS))
	return c, fmt.Errorf("etw.StartCapture: %w", ErrUnsupportedOS)
}

// NetworkBytes always reports UNMEASURED off Windows — never a measured zero.
// A fabricated zero here would draw a flat "no network traffic" line for every
// process on the box.
func (c *Capture) NetworkBytes(int) (in, out int64, ok bool) { return 0, 0, false }

// Forget is a no-op off Windows.
func (c *Capture) Forget(int) {}

// Retire is a no-op off Windows.
func (c *Capture) Retire(int) {}

// Status reports the capture's accounting mode and reason. Off Windows that is
// NetworkAccountingUnavailable after StartCapture, and NetworkAccountingOff for
// the zero value or a nil receiver (nobody requested a capture).
func (c *Capture) Status() (mode, reason string) {
	if c == nil {
		return processobs.NetworkAccountingOff, ""
	}
	return c.status.get()
}

// DecodeStats always reports ok=false off Windows: no trace session exists, so
// there is nothing whose decode health could be reported. Returning zeroed
// counters with ok=true would claim that a decoder ran and refused nothing —
// the same fabrication NetworkBytes refuses to make about bytes. It would also
// be read as "nothing was classified", because a zero Ignored beside a zero
// Decoded is what a decoder that never ran looks like; ok=false keeps that
// out of every surface at the source.
func (c *Capture) DecodeStats() (processobs.CapturerDecodeStats, bool) {
	return processobs.CapturerDecodeStats{}, false
}

// Close is a no-op off Windows.
func (c *Capture) Close() error { return nil }
