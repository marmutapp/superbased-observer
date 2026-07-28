//go:build windows

package etw

import (
	"fmt"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// Capture is a running ETW network capture: a Session pumping decoded TCP
// events into an Accumulator, exposed as cumulative per-pid byte totals.
//
// It is the value that satisfies processobs.NetworkSampler, and it is the ONLY
// thing in this package that should be wired into poll.Options.NetworkBytes —
// see Accumulator.NetworkBytes for why the accumulator itself must not be,
// even though it structurally fits.
//
// The zero value is a valid, inert Capture: Status reports "off", NetworkBytes
// reports unmeasured, Close is a no-op. So is a nil *Capture. That is what lets
// a caller keep a Capture field it may never populate without nil-checking
// every use.
type Capture struct {
	acc  *Accumulator
	sess *Session

	status captureStatus

	// done closes when the pump goroutine has returned, so Close can wait for
	// it and callers get a genuinely quiesced capture rather than a
	// still-running callback.
	done      chan struct{}
	closeOnce sync.Once
}

// StartCapture starts an ETW session, wires it into a fresh Accumulator, and
// begins pumping events on its own goroutine. Options' handlers are replaced by
// captureOptions: TCP is wired to the accumulator, UDP is severed.
//
// IT RETURNS A NON-NIL *Capture EVEN ON ERROR, and that is deliberate. The
// error is authoritative and callers should log it; but the failure this
// feature will actually hit is "not elevated", and §0.4 of the plan records
// that today such an operator gets silence. Handing back a Capture whose Status
// says "unavailable — not elevated …" and whose NetworkBytes honestly reports
// UNMEASURED means a fail-open caller that keeps the value still surfaces the
// reason, instead of dropping it on the floor. A caller that discards the value
// on error loses nothing: every method is nil-safe.
func StartCapture(opts Options) (*Capture, error) {
	c := &Capture{acc: NewAccumulator(DefaultMaxEntries), done: make(chan struct{})}

	sess, err := NewSession(captureOptions(opts, c.acc))
	if err != nil {
		c.status.set(processobs.NetworkAccountingUnavailable, captureUnavailableReason(err))
		close(c.done)
		return c, fmt.Errorf("etw.StartCapture: %w", err)
	}

	c.sess = sess
	c.status.set(processobs.NetworkAccountingTCP, "")
	go c.pump()
	return c, nil
}

// pump runs ProcessTrace, which blocks until Close stops the session.
//
// A non-nil error here means the session died on its own — the capture is over
// and every subsequent byte total would be frozen at its last value. Reporting
// that as a degradation is the difference between an operator seeing a stale
// chart and an operator being told why. degradeIfLive makes the normal
// shutdown path (Close already set "off") immune to being relabelled by this
// goroutine's return.
func (c *Capture) pump() {
	defer close(c.done)
	if err := c.sess.Process(); err != nil {
		c.status.degradeIfLive("the ETW session stopped unexpectedly — " + err.Error())
	}
}

// NetworkBytes implements processobs.NetworkSampler: CUMULATIVE TCP payload
// bytes received and sent by a pid since this capture started.
//
// It implements processobs.NetworkBytesFunc's contract EXACTLY, which means the
// two zero-ish answers are never conflated:
//
//   - ok=false — accounting is not live (never started, failed to start, died,
//     or closed). The sample must be recorded as UNMEASURED. Drawing a zero
//     here would be a fabricated observation.
//   - ok=true with (0,0) — accounting IS live and this process has moved no TCP
//     bytes. That is a real measurement, including for a pid the accumulator
//     has never seen: an idle process emits no events, so "unknown to the
//     accumulator" and "measured zero" are the same state.
//
// The second bullet is why this method cannot just forward the accumulator's
// ok. See Accumulator.NetworkBytes.
func (c *Capture) NetworkBytes(pid int) (in, out int64, ok bool) {
	if c == nil {
		return 0, 0, false
	}
	if mode, _ := c.status.get(); mode != processobs.NetworkAccountingTCP {
		return 0, 0, false
	}
	if in, out, hit := c.acc.NetworkBytes(pid); hit {
		return in, out, true
	}
	return 0, 0, true // measured, no TCP bytes yet
}

// Forget drops every counter held for a pid. Call it on EXEC: Windows reuses
// pids, and without this a new process could inherit the previous occupant's
// totals and chart a fabricated spike. Delegates to Accumulator.Forget.
func (c *Capture) Forget(pid int) {
	if c == nil {
		return
	}
	c.acc.Forget(pid)
}

// Retire moves a pid's totals into the bounded recently-exited cache. Call it
// on EXIT, so the exit sample — which the poll backend takes up to one interval
// after the process is gone — still carries the final byte counts instead of
// dropping to zero. Delegates to Accumulator.Retire.
func (c *Capture) Retire(pid int) {
	if c == nil {
		return
	}
	c.acc.Retire(pid)
}

// Status reports the capture's accounting mode and reason in the
// processobs.NetworkAccounting* vocabulary:
//
//   - NetworkAccountingTCP while the session is live (TCP payload bytes only,
//     both address families).
//   - NetworkAccountingUnavailable, with a reason that names the cause — "not
//     elevated — …" for the common case — when the session could not start or
//     died.
//   - NetworkAccountingOff when no capture was requested (the zero value or a
//     nil receiver) or after Close.
func (c *Capture) Status() (mode, reason string) {
	if c == nil {
		return processobs.NetworkAccountingOff, ""
	}
	return c.status.get()
}

// DecodeStats reports the underlying trace session's decode counters as the
// backend-agnostic processobs.CapturerDecodeStats, so the ETW types never
// spread past this package (CLAUDE.md rule 2).
//
// ok=false means THERE IS NO SESSION to report on — a nil receiver, the zero
// value, or a StartCapture whose session never came up (the common
// non-elevated run). It is not "zero events were dropped": nothing was
// decoded at all, and a caller that rendered that as a clean zero would be
// claiming the payload-length assumptions were tested and held when they were
// never exercised. That is the same absence-vs-zero trap as
// NetworkBytes' ok, and it is why this returns a bool rather than a bare
// struct — Go cannot conditionally implement a method, so the presence fact
// has to be a value.
//
// ok=true with both REFUSAL counters zero is a real measurement, but it is
// only half of the state the elevated validation is looking for. The other
// half is Decoded/Ignored, carried here for one reason: Classify routes every
// unrecognised event id to ClassIgnored, so a provider whose event ids were
// renumbered produces a report with zero drops, zero unsupported versions and
// zero bytes — a PASS on every refusal-shaped check while the decoder
// measures nothing. Decoded and Ignored are what make that shape nameable
// (processobs.CapturerDecodeStats.NothingClassified); neither is a fault on
// its own, and Ignored in particular is large on every healthy run.
//
// A session that has DIED still reports its final counters: what it dropped
// before it stopped is history the operator needs, not something to erase.
func (c *Capture) DecodeStats() (processobs.CapturerDecodeStats, bool) {
	if c == nil || c.sess == nil {
		return processobs.CapturerDecodeStats{}, false
	}
	// The projection itself lives in the untagged file so a Linux test can
	// pin every field of it — see decodeStatsOf for why that matters more
	// here than anywhere else in this package.
	return decodeStatsOf(c.sess.Stats()), true
}

// Close stops the session and waits for the pump goroutine to return. It is
// idempotent, safe on the zero value and on a nil receiver, and safe on a
// Capture whose StartCapture failed.
//
// The status flips to "off" BEFORE the session is stopped so the pump's return
// is seen as the requested shutdown it is, not as a degradation.
func (c *Capture) Close() error {
	if c == nil {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		c.status.set(processobs.NetworkAccountingOff, "capture closed")
		if c.sess != nil {
			err = c.sess.Close()
		}
		if c.done != nil {
			<-c.done
		}
	})
	return err
}
