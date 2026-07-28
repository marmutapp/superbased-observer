package etw

import (
	"errors"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// Capture is the package's processobs.NetworkSampler, pinned at COMPILE time
// rather than by a test.
//
// The reason is the same one that made W1 use two-sided size assertions: CI has
// no Windows runner and `GOOS=windows go build` does not compile _test.go
// files, so a test-only assertion would never check the Windows implementation
// at all. This var sits in the untagged file, so BOTH capture_windows.go's
// Capture and capture_other.go's stub have to satisfy the interface, and the
// existing cross-compile gate is what enforces it.
var _ processobs.NetworkSampler = (*Capture)(nil)

// The decode-health capability is pinned the same way and for the same reason:
// it is the ONLY path by which "the payload-length assumption is wrong on this
// host" (a non-zero Stats.Dropped) reaches any surface, and a signature that
// drifted on the Windows side would silently take that path away again. The
// interface is spelled inline rather than exported from processobs because
// cmd/observer's own etwNetworkCapture seam is the real consumer; this var
// exists purely so `GOOS=windows go build` — the only Windows gate there is —
// has to typecheck the Windows implementation against it.
//
// The (stats, ok) shape is load-bearing: see Capture.DecodeStats.
var _ interface {
	DecodeStats() (processobs.CapturerDecodeStats, bool)
} = (*Capture)(nil)

// decodeStatsOf projects a Session's decode counters onto the backend-agnostic
// value that crosses the transport, so the ETW types never spread past this
// package (CLAUDE.md rule 2).
//
// IT LIVES IN THE UNTAGGED FILE ON PURPOSE, and that is a direct lesson from
// the bug this function exists to stop repeating. The projection used to sit
// inside capture_windows.go, where it silently dropped Ignored — and CI could
// never have caught it, because `GOOS=windows go build` does not compile
// _test.go files and there is no Windows runner. Stats is declared
// field-for-field identically on both OSes (session_other.go says so in as
// many words), so the projection is genuinely portable and belongs where a
// Linux test can pin every field of it. A counter that is computed but never
// carried is invisible in exactly the same way a counter that is never
// computed is.
func decodeStatsOf(s Stats) processobs.CapturerDecodeStats {
	return processobs.CapturerDecodeStats{
		NetworkDropped:            s.Dropped,
		NetworkUnsupportedVersion: s.UnsupportedVersion,
		NetworkDecoded:            s.Decoded,
		NetworkIgnored:            s.Ignored,
	}
}

// captureOptions binds a caller's Options to an Accumulator: OnTCP is wired to
// the accumulator, and OnUDP is FORCED to nil.
//
// Both halves are deliberate.
//
// Wiring OnTCP here rather than inside the Windows file is what makes the
// TCP-only guarantee testable at all — CI has no Windows runner, so anything
// that lives behind //go:build windows is asserted by nobody. This function is
// untagged, so capture_test.go pins the wiring on Linux.
//
// Forcing OnUDP to nil is the scope guarantee from the package doc, enforced
// rather than documented: Linux counts TCP payload bytes only, and
// processobs.MetricSample's network fields are documented as TCP-only. A UDP
// handler on a Capture's session could only ever widen that field's meaning
// with nothing saying so. Session drops UDP events (counting them in
// Stats.Ignored) whenever OnUDP is nil, so nulling it here severs the path
// before an event is ever decoded. A caller that genuinely wants UDP must use
// NewSession directly and give the bytes their own scope discriminator.
//
// The caller's own OnTCP is likewise replaced, not chained: Capture owns the
// accumulation, and a caller-supplied handler that decided to skip events would
// silently desynchronise the totals from the wire.
func captureOptions(opts Options, acc *Accumulator) Options {
	opts.OnTCP = func(ev TCPDataEvent) { acc.Add(ev.PID, ev.Direction, ev.Bytes) }
	opts.OnUDP = nil
	return opts
}

// captureUnavailableReason renders a start failure as an operator-facing
// reason string for Status.
//
// The elevation case is called out by name because it is the failure mode this
// feature will actually hit — controlling an ETW session needs Administrator,
// the Performance Log Users group, or a LocalSystem/LocalService service — and
// because §0.4 of the plan records that today such an operator gets silence.
// "unavailable" with no reason would reproduce exactly that.
func captureUnavailableReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNeedsElevation):
		return "not elevated — " + err.Error()
	case errors.Is(err, ErrUnsupportedOS):
		return "unsupported OS — " + err.Error()
	default:
		return err.Error()
	}
}

// captureStatus is a Capture's concurrency-safe mode/reason pair, in the
// processobs.NetworkAccounting* vocabulary.
//
// It mirrors processobs.NetworkAccounting rather than embedding it: that type
// is owned and written by the backend the daemon composes, and a Capture may be
// constructed before (or without) one. Keeping a local copy means Status is
// answerable from the Capture alone, and the daemon can forward it into the
// shared handle at the seam.
//
// It lives in the untagged file for the same reason sessionHandles does: the
// ETW pump goroutine writes it while the metric sampler reads it, so it is
// genuinely concurrent state, and state behind //go:build windows can never be
// race-tested by CI.
//
// The zero value reports NetworkAccountingOff, which is the honest reading of
// "nobody has started a capture": not requested.
type captureStatus struct {
	mu     sync.Mutex
	mode   string
	reason string
}

// set records the mode and reason unconditionally.
func (s *captureStatus) set(mode, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.mode, s.reason = mode, reason
	s.mu.Unlock()
}

// get reports the mode and reason. A nil receiver, and an unset mode, both
// report NetworkAccountingOff.
func (s *captureStatus) get() (mode, reason string) {
	if s == nil {
		return processobs.NetworkAccountingOff, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "" {
		return processobs.NetworkAccountingOff, ""
	}
	return s.mode, s.reason
}

// degradeIfLive flips a LIVE capture to unavailable and reports whether it did.
//
// The conditional is the point. The ETW pump returns when the session ends, and
// that happens for two very different reasons: the session died (a real
// degradation the operator must see) or Close asked it to stop (the normal
// shutdown, already recorded as "off"). An unconditional set would let the
// pump's return overwrite a clean "off" with a scary "unavailable" moments
// later, purely on goroutine timing.
func (s *captureStatus) degradeIfLive(reason string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != processobs.NetworkAccountingTCP {
		return false
	}
	s.mode, s.reason = processobs.NetworkAccountingUnavailable, reason
	return true
}
