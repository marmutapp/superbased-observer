package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// fixedTime is in UTC so it survives a JSON (RFC3339Nano) round-trip exactly —
// no monotonic reading, no location drift.
var fixedTime = time.Date(2026, 6, 17, 17, 28, 0, 123456789, time.UTC)

// sampleFrames exercises every kind plus Windows-shaped paths/argv (backslash
// literals are host-agnostic, so this is Windows-host-test-safe).
func sampleFrames() []Frame {
	return []Frame{
		{V: WireVersion, Kind: KindHello, Hello: &Hello{Backend: "poll", BootID: "boot-abc", OS: "windows", PID: 4242}},
		{V: WireVersion, Kind: KindEvent, Event: &processobs.RawEvent{
			Type: processobs.EventFork, Timestamp: fixedTime,
			BootID: "boot-abc", PID: 1000, PPID: 4, StartTimeTicks: 132000000000000000, HasStartTime: true,
		}},
		{V: WireVersion, Kind: KindEvent, Event: &processobs.RawEvent{
			Type: processobs.EventExec, Timestamp: fixedTime,
			BootID: "boot-abc", PID: 1000, PPID: 4, StartTimeTicks: 132000000000000000, HasStartTime: true,
			ExePath:      `C:\Program Files\nodejs\node.exe`,
			Argv:         []string{"node.exe", "--inspect", `C:\Users\marmu\proj\server.js`, "tök=ünïcode"},
			CWD:          `C:\Users\marmu\proj`,
			SessionToken: "19f16087-7d3d-40f9-aec0-f59c3849447b", // §5.5 P-B6 env-token must survive the wire
		}},
		{V: WireVersion, Kind: KindEvent, Event: &processobs.RawEvent{
			Type: processobs.EventExit, Timestamp: fixedTime,
			BootID: "boot-abc", PID: 1000, StartTimeTicks: 132000000000000000, HasStartTime: true,
			ExitCode: 1,
		}},
		{V: WireVersion, Kind: KindError, Error: "transient enumerate failure"},
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	in := sampleFrames()
	for _, f := range in {
		var err error
		switch f.Kind {
		case KindHello:
			err = enc.Hello(*f.Hello)
		case KindEvent:
			err = enc.Event(*f.Event)
		case KindError:
			err = enc.Errorf("%s", f.Error)
		}
		if err != nil {
			t.Fatalf("encode %s: %v", f.Kind, err)
		}
	}

	dec := NewDecoder(&buf)
	var got []Frame
	for {
		f, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, f)
	}

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got=%#v\nwant=%#v", got, in)
	}
}

func TestEncodeIsLineDelimitedNDJSON(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, f := range sampleFrames() {
		if err := enc.write(f); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("stream must end in a newline; got %q", out[len(out)-1:])
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(sampleFrames()) {
		t.Fatalf("want %d NDJSON lines, got %d", len(sampleFrames()), len(lines))
	}
	for i, ln := range lines {
		if strings.Contains(ln, "\n") {
			t.Fatalf("line %d contains an embedded newline", i)
		}
		if !strings.HasPrefix(ln, "{") || !strings.HasSuffix(ln, "}") {
			t.Fatalf("line %d is not a single JSON object: %q", i, ln)
		}
	}
}

func TestDecodeMalformedLineContinues(t *testing.T) {
	// A garbage line between two valid frames: Next returns a non-EOF error for
	// the bad line, then the following Next yields the next valid frame.
	stream := `{"v":1,"kind":"error","error":"first"}` + "\n" +
		"this is not json" + "\n" +
		`{"v":1,"kind":"error","error":"third"}` + "\n"

	dec := NewDecoder(strings.NewReader(stream))

	f1, err := dec.Next()
	if err != nil || f1.Error != "first" {
		t.Fatalf("frame 1: got (%+v, %v)", f1, err)
	}

	_, err = dec.Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("malformed line: want a non-EOF decode error, got %v", err)
	}

	f3, err := dec.Next()
	if err != nil || f3.Error != "third" {
		t.Fatalf("frame 3 (after bad line): got (%+v, %v)", f3, err)
	}

	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF at stream end, got %v", err)
	}
}

func TestDecodeSkipsBlankLines(t *testing.T) {
	stream := "\n\n" + `{"v":1,"kind":"hello","hello":{"backend":"poll","os":"windows"}}` + "\n\n"
	dec := NewDecoder(strings.NewReader(stream))
	f, err := dec.Next()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Kind != KindHello || f.Hello == nil || f.Hello.Backend != "poll" {
		t.Fatalf("unexpected frame: %+v", f)
	}
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestEveryFrameStampsWireVersion(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	_ = enc.Hello(Hello{Backend: "poll"})
	_ = enc.Event(processobs.RawEvent{Type: processobs.EventExec})
	_ = enc.Errorf("x")

	dec := NewDecoder(&buf)
	for {
		f, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if f.V != WireVersion {
			t.Fatalf("frame %s has v=%d, want %d", f.Kind, f.V, WireVersion)
		}
	}
}

// TestCapturerStatsWireBackCompatBothDirections pins design constraint 5: the
// classification counters are ADDITIVE, so neither half of a version-skewed
// pair breaks.
//
// The conservative direction is the point. An older capturer omits the keys,
// they decode as zero, and a zero ignored count fires NOTHING — the report
// reads as "not proven", never as a fabricated pass and never as a false
// alarm about a provider that is fine.
func TestCapturerStatsWireBackCompatBothDirections(t *testing.T) {
	t.Parallel()

	// OLD CAPTURER → NEW DAEMON: the two new keys are simply absent.
	const oldLine = `{"v":1,"kind":"stats","stats":{"network_decode_dropped":0,"network_decode_unsupported_version":0}}`
	var f Frame
	if err := json.Unmarshal([]byte(oldLine), &f); err != nil {
		t.Fatalf("a pre-E6b stats line no longer decodes: %v", err)
	}
	if f.Stats == nil {
		t.Fatal("stats frame decoded to nil")
	}
	if err := f.Stats.validate(); err != nil {
		t.Fatalf("a pre-E6b report was refused: %v", err)
	}
	decode := f.Stats.DecodeStats()
	if decode.NetworkIgnored != 0 || decode.NetworkDecoded != 0 {
		t.Fatalf("absent keys did not decode as zero: %+v", decode)
	}
	if decode.Any() || decode.NothingClassified() {
		t.Fatalf("an older capturer's report raised a signal it cannot support: %+v", decode)
	}

	// NEW CAPTURER → OLD DAEMON: the older daemon's struct has only the two
	// original fields, so the new keys are unknown and must be ignored rather
	// than failing the frame.
	type legacyCapturerStats struct {
		NetworkDecodeDropped            int64 `json:"network_decode_dropped"`
		NetworkDecodeUnsupportedVersion int64 `json:"network_decode_unsupported_version"`
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Stats(CapturerStats{
		NetworkDecodeDropped: 3, NetworkDecodeUnsupportedVersion: 1,
		NetworkDecodeDecoded: 900, NetworkDecodeIgnored: 48_211,
	}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var legacyFrame struct {
		V     int                  `json:"v"`
		Kind  FrameKind            `json:"kind"`
		Stats *legacyCapturerStats `json:"stats"`
	}
	if err := json.Unmarshal(buf.Bytes(), &legacyFrame); err != nil {
		t.Fatalf("a new stats line broke an older daemon's decode: %v", err)
	}
	if legacyFrame.Stats == nil || legacyFrame.Stats.NetworkDecodeDropped != 3 ||
		legacyFrame.Stats.NetworkDecodeUnsupportedVersion != 1 {
		t.Fatalf("the fields an older daemon DOES read did not survive: %+v", legacyFrame.Stats)
	}
}
