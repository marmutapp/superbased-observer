package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// WireVersion is the bridge NDJSON protocol version. The capturer stamps it on
// every Frame; the decoder records it from the hello frame so the backend can
// validate compatibility. Unknown JSON fields are ignored (forward-compatible),
// so bump this only on an INCOMPATIBLE change to the frame semantics.
const WireVersion = 1

// maxLineBytes bounds a single NDJSON line. A RawEvent carries raw (unscrubbed)
// argv, so the budget is generous; a longer line is a decode error, never a
// crash or an OOM.
const maxLineBytes = 1 << 20 // 1 MiB

// FrameKind discriminates a line on the wire.
type FrameKind string

const (
	// KindHello is the first line the capturer emits: protocol version +
	// capturer identity, so the decoder can validate compatibility before
	// processing events.
	KindHello FrameKind = "hello"
	// KindEvent carries one process RawEvent.
	KindEvent FrameKind = "event"
	// KindError is a non-fatal capturer-side diagnostic (e.g. a transient
	// enumerate failure). The decoder surfaces it to health and keeps reading —
	// it is NOT a stream end.
	KindError FrameKind = "error"
	// KindStats carries the capturer's own decoder counters, re-sent
	// periodically for the life of the stream.
	//
	// It is a separate frame rather than fields on Hello because the numbers
	// are ZERO at hello time and only become interesting later: a hello-only
	// report would tell the operator about drops one reconnect too late, and
	// on a healthy long-lived link never at all. It is ADDITIVE — a daemon
	// that predates it ignores the unknown kind (consumeFrames' switch has no
	// default) and a capturer that predates it simply never sends one — so
	// WireVersion deliberately does NOT move for it.
	KindStats FrameKind = "stats"
)

// Hello is the capturer's opening announcement.
type Hello struct {
	// Backend names the event source. "poll" is the lifecycle-only capturer;
	// "poll+etw" is that same lifecycle stream with an ETW network capture
	// supplying per-process byte counters on top. The name states what
	// actually ran: a capturer asked for ETW that could not start the trace
	// session reports plain "poll", because that is where its events came
	// from.
	Backend string `json:"backend"`
	// BootID stamps the capturer host's boot, the first component of every
	// ProcessKey (§9.3). Carried so the WSL side need not re-derive it.
	BootID string `json:"boot_id"`
	// OS is the capturer's runtime.GOOS ("windows").
	OS string `json:"os"`
	// PID is the capturer process pid, for diagnostics only.
	PID int `json:"pid"`

	// NetworkAccountingMode reports whether the capturer is measuring
	// per-process network bytes, in the processobs.NetworkAccounting*
	// vocabulary ("off" / "unavailable" / "tcp"). It carries the far side's
	// truth across the bridge so a Windows deployment's health is the Windows
	// capturer's health, not the daemon host's guess.
	//
	// It is OPTIONAL and omitted when empty. A pre-W2 capturer never sends it,
	// and an omitted mode means UNKNOWN — the consumer must leave its own
	// status untouched rather than infer a confident "off". Adding an optional
	// field is not an incompatible change, so WireVersion does NOT move for it.
	//
	// It is UNTRUSTED INPUT on the receiving side and is validated against
	// processobs.KnownNetworkAccountingMode at the boundary (netClaim.apply).
	NetworkAccountingMode string `json:"network_accounting_mode,omitempty"`
	// NetworkAccountingReason is the human-readable explanation of a non-live
	// mode ("ETW session start failed: Access is denied …"). Optional, and
	// meaningless without NetworkAccountingMode.
	//
	// Also UNTRUSTED INPUT: it reaches a doctor line and a Prometheus label,
	// so the receiver clamps it at the boundary (netClaim.apply). Nothing on
	// the wire bounds it but the 1 MiB NDJSON line budget.
	NetworkAccountingReason string `json:"network_accounting_reason,omitempty"`
}

// IsMeasuringNetworkMode reports whether a NetworkAccountingMode is a POSITIVE
// claim that per-process bytes are actually being counted.
//
// It is a property of the mode vocabulary, not a list of known-good names: any
// mode that is neither "unknown" (empty), nor "off", nor "unavailable" asserts
// live measurement, so a later scope (say a TCP+UDP mode) is covered without
// editing this. The distinction matters because only a positive claim goes
// stale when the capturer dies — "off" stays true whether or not anything is
// running.
//
// BECAUSE the rule is open-ended, it is only sound over a mode this build
// DEFINES. Callers must first put a mode that came off the wire through
// processobs.KnownNetworkAccountingMode (netClaim.apply does), or a remote
// could assert a positive measurement claim simply by inventing a name.
func IsMeasuringNetworkMode(mode string) bool {
	switch mode {
	case "", processobs.NetworkAccountingOff, processobs.NetworkAccountingUnavailable:
		return false
	default:
		return true
	}
}

// CapturerStats is the capturer's periodic self-report of its OS network-
// telemetry decoder's health. It is the ONLY path by which a non-zero drop
// count — "the fixed-offset payload layout does not hold on this host, so the
// byte totals are wrong with no error anywhere" — can reach the daemon's
// surfaces at all.
//
// A capturer sends it ONLY when it actually has a decoder running. A run that
// asked for the elevated network capture and did not get it (the common
// non-elevated case) sends NOTHING, so the daemon's presence flag stays false
// and every surface renders absence. Sending zeroes there would assert that
// the layout assumptions were exercised and held.
//
// It is UNTRUSTED INPUT on the receiving side even though it arrives after
// authentication — the accept listener's bind is reachable from every process
// on the Windows host under WSL localhostForwarding — so it is validated at the
// decode boundary (validate) before any sink sees it, and a report that fails
// is REFUSED rather than repaired.
type CapturerStats struct {
	// NetworkDecodeDropped is the capturer's count of network data events its
	// decoder REFUSED as short or unexpectedly shaped.
	NetworkDecodeDropped int64 `json:"network_decode_dropped"`
	// NetworkDecodeUnsupportedVersion is its count of data events refused
	// because the provider stamped an event version the capturer's layout
	// table does not describe.
	NetworkDecodeUnsupportedVersion int64 `json:"network_decode_unsupported_version"`
	// NetworkDecodeDecoded is its count of data events ACCEPTED and handed to
	// the byte accumulator, and NetworkDecodeIgnored its count of events
	// classified as not-a-data-event (control-plane, connect/disconnect,
	// retransmit, UDP with no handler).
	//
	// They travel together because neither means anything alone and both are
	// needed for the one conjunction that catches a renumbered provider (see
	// processobs.CapturerDecodeStats.NothingClassified). A large ignored
	// count is NORMAL; what is not normal is a large ignored count beside a
	// zero decoded count.
	//
	// BOTH ARE ADDITIVE FIELDS, exactly like the two above them: a capturer
	// built before this change omits them and json.Unmarshal leaves them at
	// zero, and a newer capturer talking to an older daemon has them ignored
	// as unknown keys. The absent case therefore reads as decoded 0 /
	// ignored 0, which fires NOTHING (NothingClassified needs a non-zero
	// ignored count) — the conservative direction: a version-skewed pair
	// reads as "not proven", never as a fabricated pass or a false alarm.
	NetworkDecodeDecoded int64 `json:"network_decode_decoded"`
	NetworkDecodeIgnored int64 `json:"network_decode_ignored"`
}

// validate decides whether a remote-supplied stats frame is a report this
// daemon can stand behind at all. A refused frame is REFUSED, not repaired.
//
// It replaces an earlier normalize() that floored a negative counter at zero,
// and the change is the whole point: this surface exists to distinguish
// "reported zero" (the decoder ran and refused nothing — a PASS) from "never
// reported" (no decoder ran — not a pass). Quietly rewriting a nonsensical
// report into all-zeros manufactures the pass. A count of events cannot run
// backwards, so a negative is a broken report, and broken is a third state
// that must not be laundered into either of the other two. The caller counts
// it as a refused line, which is loud in exactly the way silence is not.
//
// UPPER BOUNDS ARE DELIBERATELY ABSENT, because there is no knowable ceiling on
// how many events a decoder may have refused, and inventing one would refuse
// real reports from a genuinely broken host — the reports that matter most.
// What bounds the input instead is structural and already enforced: the 1 MiB
// NDJSON line budget, json.Unmarshal rejecting anything that does not fit an
// int64 (an over-large, fractional or non-numeric value fails the whole frame
// rather than truncating — measured), the 64-consecutive-decode-error abandon,
// and the fact that NOTHING accumulates these numbers: each report REPLACES
// the last, so no arithmetic on them can overflow.
//
// It is applied ONCE, in consumeFrames, so both transports get it and neither
// has its own copy to forget (the clampRemoteText rule, applied to numbers).
// EVERY counter is validated the same way, including the two added later: a
// negative decoded or ignored count is as impossible as a negative drop
// count, and a report carrying one is broken in a way that says nothing about
// which of its OTHER numbers can be trusted. So the whole frame fails, and no
// surface sees a partially-credible report.
func (s CapturerStats) validate() error {
	if s.NetworkDecodeDropped < 0 || s.NetworkDecodeUnsupportedVersion < 0 ||
		s.NetworkDecodeDecoded < 0 || s.NetworkDecodeIgnored < 0 {
		return fmt.Errorf("negative decoder counters (dropped=%d unsupported_version=%d decoded=%d ignored=%d): "+
			"a count of events cannot run backwards",
			s.NetworkDecodeDropped, s.NetworkDecodeUnsupportedVersion,
			s.NetworkDecodeDecoded, s.NetworkDecodeIgnored)
	}
	return nil
}

// DecodeStats projects the wire shape onto the backend-agnostic value the
// health surfaces carry.
func (s CapturerStats) DecodeStats() processobs.CapturerDecodeStats {
	return processobs.CapturerDecodeStats{
		NetworkDropped:            s.NetworkDecodeDropped,
		NetworkUnsupportedVersion: s.NetworkDecodeUnsupportedVersion,
		NetworkDecoded:            s.NetworkDecodeDecoded,
		NetworkIgnored:            s.NetworkDecodeIgnored,
	}
}

// Frame is one NDJSON line. Exactly one of Hello/Event/Stats is set, per Kind
// (a KindError frame uses Error). RawEvent is reused verbatim as the event
// payload (§5.5) — no parallel wire struct to drift.
type Frame struct {
	V     int                  `json:"v"`
	Kind  FrameKind            `json:"kind"`
	Hello *Hello               `json:"hello,omitempty"`
	Event *processobs.RawEvent `json:"event,omitempty"`
	Stats *CapturerStats       `json:"stats,omitempty"`
	Error string               `json:"error,omitempty"`
}

// Encoder writes Frames as NDJSON to an io.Writer, one JSON object per line and
// flushed immediately so the consumer receives each line as it is produced (the
// streaming property P-B0 verified). Safe for concurrent use: each frame is a
// single locked write so lines never interleave.
type Encoder struct {
	mu sync.Mutex
	w  *bufio.Writer
}

// NewEncoder wraps w (typically os.Stdout on the capturer).
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: bufio.NewWriter(w)}
}

// Hello emits the opening hello frame.
func (e *Encoder) Hello(h Hello) error {
	return e.write(Frame{V: WireVersion, Kind: KindHello, Hello: &h})
}

// Event emits one process-event frame.
func (e *Encoder) Event(ev processobs.RawEvent) error {
	return e.write(Frame{V: WireVersion, Kind: KindEvent, Event: &ev})
}

// Stats emits one capturer decode-health frame. Callers send it only while
// they genuinely have a decoder running — see CapturerStats.
func (e *Encoder) Stats(s CapturerStats) error {
	return e.write(Frame{V: WireVersion, Kind: KindStats, Stats: &s})
}

// Errorf emits a non-fatal capturer diagnostic frame.
func (e *Encoder) Errorf(format string, args ...any) error {
	return e.write(Frame{V: WireVersion, Kind: KindError, Error: fmt.Sprintf(format, args...)})
}

// write marshals one frame, appends a newline, and flushes.
func (e *Encoder) write(f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("bridge.Encoder: marshal: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(b); err != nil {
		return fmt.Errorf("bridge.Encoder: write: %w", err)
	}
	if err := e.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("bridge.Encoder: write: %w", err)
	}
	return e.w.Flush()
}

// Decoder reads Frames from an io.Reader (the capturer's stdout pipe), one per
// line. io.EOF marks the clean end of the stream. A malformed line returns a
// non-nil error that is NOT io.EOF — the caller counts it (decode-error health)
// and calls Next again, since the stream is still live (the scanner has already
// advanced past the bad line). bufio's ScanLines strips a trailing \r, so a
// CRLF-translated stream decodes cleanly.
type Decoder struct {
	sc *bufio.Scanner
}

// NewDecoder reads NDJSON frames from r.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	return &Decoder{sc: sc}
}

// Next returns the next frame, io.EOF at the clean end of the stream, or a
// non-EOF error for a malformed/oversized line (the caller should count and
// continue). Blank lines are skipped defensively.
func (d *Decoder) Next() (Frame, error) {
	for {
		if !d.sc.Scan() {
			if err := d.sc.Err(); err != nil {
				return Frame{}, fmt.Errorf("bridge.Decoder: read: %w", err)
			}
			return Frame{}, io.EOF
		}
		line := bytes.TrimSpace(d.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return Frame{}, fmt.Errorf("bridge.Decoder: decode line: %w", err)
		}
		return f, nil
	}
}
