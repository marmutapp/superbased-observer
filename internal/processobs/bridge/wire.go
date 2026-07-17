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
)

// Hello is the capturer's opening announcement.
type Hello struct {
	// Backend names the event source ("poll"; later "etw").
	Backend string `json:"backend"`
	// BootID stamps the capturer host's boot, the first component of every
	// ProcessKey (§9.3). Carried so the WSL side need not re-derive it.
	BootID string `json:"boot_id"`
	// OS is the capturer's runtime.GOOS ("windows").
	OS string `json:"os"`
	// PID is the capturer process pid, for diagnostics only.
	PID int `json:"pid"`
}

// Frame is one NDJSON line. Exactly one of Hello/Event is set, per Kind (a
// KindError frame uses Error). RawEvent is reused verbatim as the event
// payload (§5.5) — no parallel wire struct to drift.
type Frame struct {
	V     int                  `json:"v"`
	Kind  FrameKind            `json:"kind"`
	Hello *Hello               `json:"hello,omitempty"`
	Event *processobs.RawEvent `json:"event,omitempty"`
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
