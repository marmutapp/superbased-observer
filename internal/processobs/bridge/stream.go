package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// frameSink is everything the shared decode loop needs from the transport that
// owns it. It exists so the NDJSON consumption rules — version check, decode-
// error tolerance, hello handling, the consumer-gone exit — live in ONE place
// while the transports around them differ completely: the spawn Backend owns a
// child process and a stderr tail, the accept Listener owns a socket and an
// authentication handshake. Only the transport is per-mode; the stream is not
// (docs/plans/process-obs-etw-windows-parity-plan-2026-07-26.md §W3).
//
// Deliberately unexported: it is an internal seam between two files of this
// package, not a public extension point.
type frameSink interface {
	// onHello receives a decoded hello frame.
	onHello(h Hello)
	// onDecodeErr reports a malformed/oversized line. The stream is still
	// live; the loop continues.
	onDecodeErr(err error)
	// onWireMismatch reports a hello whose protocol version is not ours.
	onWireMismatch(got, want int)
	// onErrorFrame reports a capturer-side diagnostic (KindError). NOT a
	// stream end.
	onErrorFrame(msg string)
	// onCapturerStats reports the capturer's own decoder counters
	// (KindStats). Already normalised — the transport receives values this
	// build can stand behind, never raw remote input.
	onCapturerStats(s CapturerStats)
	// onEvent delivers one process event. false means the consumer is gone
	// (ctx cancelled / backend closed) and the loop must stop.
	onEvent(ctx context.Context, ev processobs.RawEvent) bool
}

// maxConsecutiveDecodeErrs bounds a stream that yields nothing but
// undecodable lines.
//
// It is a STUCK-SCANNER guard, not a quality bar. Decoder.Next reports a
// per-line JSON failure and a terminal scanner failure (e.g. bufio.ErrTooLong
// on a line past maxLineBytes) as the same shape of non-EOF error, but only
// the first is recoverable: once bufio.Scanner records an error it returns
// that same error forever, so "skip the line and keep reading" becomes an
// infinite hot loop. Real streams never approach this count — a run of this
// many consecutive failures with no frame in between means the stream is
// unusable, not merely noisy.
const maxConsecutiveDecodeErrs = 64

// consumeFrames decodes the NDJSON frame stream from r into sink until the
// stream ends or the sink reports its consumer is gone. It is
// transport-agnostic by construction — r is an os/exec stdout pipe on the
// spawn path and a net.Conn on the accept path, and this function can tell the
// difference from neither.
//
// It returns the number of EVENT frames forwarded, whether it stopped because
// the consumer went away (the caller then tears the transport down: kill the
// child, or close the socket), and the terminal transport error when the
// stream DIED rather than ended (nil on a clean EOF).
func consumeFrames(ctx context.Context, r io.Reader, sink frameSink) (events int64, consumerGone bool, streamErr error) {
	// A dead transport is the END of this stream, not a corrupt line in it —
	// so the underlying error is recorded and the scanner is shown a clean
	// EOF. Without this a socket reset (the NORMAL way an accept-mode capturer
	// goes away, where a pipe would simply EOF) surfaces as an unrecoverable
	// decode error the loop would otherwise retry forever.
	sr := &streamReader{r: r}
	dec := NewDecoder(sr)
	errStreak := 0
	for {
		frame, err := dec.Next()
		if errors.Is(err, io.EOF) {
			return events, false, sr.err
		}
		if err != nil {
			errStreak++
			if errStreak >= maxConsecutiveDecodeErrs {
				return events, false, fmt.Errorf("bridge: %d consecutive undecodable lines, abandoning the stream: %w", errStreak, err)
			}
			sink.onDecodeErr(err)
			continue
		}
		errStreak = 0
		switch frame.Kind {
		case KindHello:
			if frame.V != WireVersion {
				sink.onWireMismatch(frame.V, WireVersion)
			}
			if frame.Hello != nil {
				sink.onHello(*frame.Hello)
			}
		case KindEvent:
			if frame.Event == nil {
				continue
			}
			events++
			if !sink.onEvent(ctx, *frame.Event) {
				return events, true, nil
			}
		case KindError:
			sink.onErrorFrame(frame.Error)
		case KindStats:
			if frame.Stats == nil {
				continue
			}
			// Validate HERE, once, so neither transport carries its own copy
			// of the untrusted-input rule (the clampRemoteText posture,
			// applied to numbers). An unusable report is reported as a
			// REFUSED LINE rather than repaired into a plausible one: this
			// surface's whole job is telling "reported zero" from "never
			// reported", and a laundered report would fabricate the first.
			// It does not end the stream — the next heartbeat may be fine.
			if err := frame.Stats.validate(); err != nil {
				sink.onDecodeErr(fmt.Errorf("bridge: refusing a capturer decoder report: %w", err))
				continue
			}
			sink.onCapturerStats(*frame.Stats)
		}
	}
}

// streamReader records a non-EOF read failure and presents it to the decoder
// as a clean EOF. See consumeFrames for why.
type streamReader struct {
	r   io.Reader
	err error
}

func (s *streamReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
		err = io.EOF
	}
	return n, err
}

// netClaim owns exactly one bit of state — whether the LAST hello made a
// POSITIVE claim that per-process network bytes are being counted — plus the
// two transitions on it: a hello asserts, and the capturer's departure
// withdraws.
//
// Both transports share this type so the stale-claim rule has ONE
// implementation and cannot drift between them (CLAUDE.md rule 4: one owner
// per piece of state). The rule itself: a claim that a capturer is measuring
// bytes dies with the capturer, so it must be withdrawn on disconnect — while
// a NON-positive mode ("off" / "unavailable") stays true whether or not a
// capturer is running and is therefore left alone, since re-badging an
// operator's deliberate "not requested" as a failure would be noise.
//
// The NetworkAccounting handle is passed in rather than held: the transport
// owns whether it has the handle at all (a bridge that is not the byte-capable
// source is handed nil), and *NetworkAccounting is nil-safe.
type netClaim struct {
	mu        sync.Mutex
	measuring bool
}

// apply carries a hello's accounting status onto handle. An omitted mode (a
// pre-W2 capturer, which never sends the field) is UNKNOWN and deliberately
// writes NOTHING: the daemon's own pessimistic default stands, because
// translating silence into a confident "off" would be a fabricated claim.
//
// BOTH hello fields are REMOTE INPUT and are treated as such, even though
// they arrive after authentication. The accept transport's bind is reachable
// from any process on the Windows host (WSL localhostForwarding), the token
// is a file on that same host, and these two strings flow verbatim into an
// operator-facing doctor line and into Prometheus labels — so the authorising
// token buys trust in the SENDER, not in the shape of what it sends. The two
// checks below are the whole of that treatment:
//
//   - the MODE must be one this build defines. IsMeasuringNetworkMode treats
//     "anything that is not off/unavailable" as a POSITIVE claim that bytes
//     are being counted, which is the right rule for a vocabulary this build
//     owns and exactly the wrong one for a string a remote picked: an
//     invented mode would otherwise assert live measurement under a name
//     nothing recognises, and land as its own metric series. An unrecognised
//     mode is therefore recorded as UNAVAILABLE — not silently dropped
//     (silence is the failure this whole surface exists to remove) and not
//     adopted — with the offending value quoted, clamped, in the reason.
//   - the REASON is clamped to the same bound the pre-auth refusal reason
//     uses. It is otherwise capped only by the 1 MiB NDJSON line budget.
func (c *netClaim) apply(handle *processobs.NetworkAccounting, h Hello) {
	if h.NetworkAccountingMode == "" {
		return
	}
	mode, reason := h.NetworkAccountingMode, clampRemoteText(h.NetworkAccountingReason)
	if !processobs.KnownNetworkAccountingMode(mode) {
		mode = processobs.NetworkAccountingUnavailable
		reason = fmt.Sprintf(
			"the cross-OS process capturer reported an unrecognised network-accounting mode %q — "+
				"treated as unmeasured; upgrade the two halves to the same observer build",
			clampRemoteText(h.NetworkAccountingMode),
		)
	}
	c.mu.Lock()
	c.measuring = IsMeasuringNetworkMode(mode)
	c.mu.Unlock()
	handle.Set(mode, reason)
}

// invalidate withdraws a STANDING positive claim, replacing it with
// "unavailable" plus reason. A non-positive claim, or a capturer that never
// reported, is a no-op — as is a second call (the transports invalidate both
// per-run and on shutdown, so idempotence is load-bearing).
func (c *netClaim) invalidate(handle *processobs.NetworkAccounting, reason string) {
	c.mu.Lock()
	measuring := c.measuring
	c.measuring = false
	c.mu.Unlock()
	if !measuring {
		return
	}
	handle.Set(processobs.NetworkAccountingUnavailable, reason)
}
