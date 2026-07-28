package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// --- fakes (thin wrappers over the package's existing launch fakes) ---

// repaintRecordingWriter wraps the package's recordingWriter fake with an
// ORDERED log of every Resize call, so a test can assert the exact winsize
// sequence the reattach repaint nudge puts on the wire (recordingWriter only
// counts). Everything else — Write, Revoked, Release, Holder, RevokeIsTakeover
// — is inherited unchanged from the existing fake.
type repaintRecordingWriter struct {
	*recordingWriter
	mu    sync.Mutex
	sizes [][2]uint16
}

// newRepaintRecordingWriter builds a sequence-recording writer lease.
func newRepaintRecordingWriter() *repaintRecordingWriter {
	return &repaintRecordingWriter{recordingWriter: newRecordingWriter()}
}

// Resize appends (rows, cols) to the ordered log, then delegates to the
// embedded recordingWriter so its counter stays authoritative too.
func (w *repaintRecordingWriter) Resize(rows, cols uint16) error {
	w.mu.Lock()
	w.sizes = append(w.sizes, [2]uint16{rows, cols})
	w.mu.Unlock()
	return w.recordingWriter.Resize(rows, cols)
}

// seq returns a copy of the recorded winsize sequence (race-safe: the bridge's
// read loop appends from its own goroutine).
func (w *repaintRecordingWriter) seq() [][2]uint16 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][2]uint16, len(w.sizes))
	copy(out, w.sizes)
	return out
}

// repaintLaunchManager reuses the package's recordingLaunchManager seam but
// hands the OWNER-LOCAL writer path a sequence-recording lease (the recording
// manager's own localWriter field is typed to the counting fake, so it cannot
// carry one). Every other manager method is promoted unchanged.
type repaintLaunchManager struct {
	*recordingLaunchManager
	writer LaunchWriter
}

// newRepaintLaunchManager wires a recording manager whose AcquireWriterLocal
// returns w, with snap as the live-session snapshot ptySizeForHandle reads.
func newRepaintLaunchManager(w LaunchWriter, snap []LaunchInfo) *repaintLaunchManager {
	inner := newRecordingLaunchManager(nil)
	inner.snapshot = snap
	return &repaintLaunchManager{recordingLaunchManager: inner, writer: w}
}

// AcquireWriterLocal grants the sequence-recording lease on the loopback path.
func (m *repaintLaunchManager) AcquireWriterLocal(string) (LaunchWriter, error) {
	m.recordingLaunchManager.localCalls.Add(1)
	return m.writer, nil
}

// --- helpers ---

// repaintGeometry is the live PTY size every reattach test starts from.
var repaintGeometry = []LaunchInfo{{ID: "HANDLE-abc", Rows: 40, Cols: 120, InitialRows: 24, InitialCols: 80}}

// dialRepaintWS opens a websocket against ts's /ws/launch/HANDLE-abc and waits
// for the on-open pty_size frame, which proves the bridge's read loop is live
// before the test writes its resize frame.
func dialRepaintWS(t *testing.T, ctx context.Context, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/launch/HANDLE-abc", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {ts.URL}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if !waitForControl(t, ctx, c, "pty_size") {
		t.Fatal("bridge never sent the on-open pty_size frame")
	}
	return c
}

// waitResizeSeq polls w until it has recorded want calls (or the deadline
// expires), then settles briefly and returns the final sequence so a test can
// also prove no EXTRA resize arrived afterwards.
func waitResizeSeq(t *testing.T, w *repaintRecordingWriter, want int) [][2]uint16 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.seq()) >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // settle: catch a late/extra nudge
	return w.seq()
}

// sameSeq compares two winsize sequences.
func sameSeq(a, b [][2]uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- pure decision table ---

// TestTerminalRepaintNudgeDecisionTable pins repaintNudgeRows, the whole
// decision for "does this resize need a forced SIGWINCH, and to what row
// count": only a first, geometry-known, size-IDENTICAL resize nudges, and the
// intermediate row count shrinks (never exceeding the client's viewport) except
// at the 1-row underflow edge.
func TestTerminalRepaintNudgeDecisionTable(t *testing.T) {
	tests := []struct {
		name          string
		before        ptyGeometry
		rows, cols    uint16
		alreadyNudged bool
		wantMid       uint16
		wantNudge     bool
	}{
		{"identical size nudges down one row", ptyGeometry{rows: 40, cols: 120}, 40, 120, false, 39, true},
		{"already nudged on this bridge", ptyGeometry{rows: 40, cols: 120}, 40, 120, true, 0, false},
		{"unknown geometry (all zero)", ptyGeometry{}, 40, 120, false, 0, false},
		{"unknown rows only", ptyGeometry{rows: 0, cols: 120}, 40, 120, false, 0, false},
		{"unknown cols only", ptyGeometry{rows: 40, cols: 0}, 40, 120, false, 0, false},
		{"rows differ — real SIGWINCH already", ptyGeometry{rows: 40, cols: 120}, 30, 120, false, 0, false},
		{"cols differ — real SIGWINCH already", ptyGeometry{rows: 40, cols: 120}, 40, 100, false, 0, false},
		{"both differ", ptyGeometry{rows: 40, cols: 120}, 30, 100, false, 0, false},
		{"two-row terminal still shrinks", ptyGeometry{rows: 2, cols: 80}, 2, 80, false, 1, true},
		{"one-row terminal bounces up (underflow guard)", ptyGeometry{rows: 1, cols: 80}, 1, 80, false, 2, true},
		{"zero requested rows never nudges", ptyGeometry{rows: 0, cols: 80}, 0, 80, false, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mid, ok := repaintNudgeRows(tc.before, tc.rows, tc.cols, tc.alreadyNudged)
			if ok != tc.wantNudge || mid != tc.wantMid {
				t.Fatalf("repaintNudgeRows = (%d, %v), want (%d, %v)", mid, ok, tc.wantMid, tc.wantNudge)
			}
		})
	}
}

// --- bridge-level pins ---

// TestTerminalRepaintNudgeOnWriterReattach is the positive pin: a reattaching
// client holding the writer lease that re-sends the PTY's CURRENT dimensions
// gets exactly ONE nudge pair — (rows-1, cols) then back — and the PTY ends at
// the original size. Without it the identical winsize raises no SIGWINCH and a
// full-screen TUI never repaints.
func TestTerminalRepaintNudgeOnWriterReattach(t *testing.T) {
	w := newRepaintRecordingWriter()
	lm := newRepaintLaunchManager(w, repaintGeometry)
	t.Cleanup(func() { close(lm.sub.release) })
	ts := httptest.NewServer(newLaunchTestServer(t, lm).Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dialRepaintWS(t, ctx, ts)
	defer func() { _ = c.CloseNow() }()

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":40,"cols":120}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	got := waitResizeSeq(t, w, 3)
	want := [][2]uint16{{40, 120}, {39, 120}, {40, 120}}
	if !sameSeq(got, want) {
		t.Fatalf("resize sequence = %v, want %v (one nudge pair, restored size)", got, want)
	}
}

// TestTerminalRepaintNudgeFiresOnlyOncePerBridge proves the nudge is one-shot:
// a client that re-sends the same no-op resize does not re-flicker every viewer
// of the PTY.
func TestTerminalRepaintNudgeFiresOnlyOncePerBridge(t *testing.T) {
	w := newRepaintRecordingWriter()
	lm := newRepaintLaunchManager(w, repaintGeometry)
	t.Cleanup(func() { close(lm.sub.release) })
	ts := httptest.NewServer(newLaunchTestServer(t, lm).Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dialRepaintWS(t, ctx, ts)
	defer func() { _ = c.CloseNow() }()

	for i := 0; i < 3; i++ {
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":40,"cols":120}`)); err != nil {
			t.Fatalf("write resize %d: %v", i, err)
		}
	}

	got := waitResizeSeq(t, w, 5)
	want := [][2]uint16{{40, 120}, {39, 120}, {40, 120}, {40, 120}, {40, 120}}
	if !sameSeq(got, want) {
		t.Fatalf("resize sequence = %v, want %v (nudge only on the FIRST no-op resize)", got, want)
	}
}

// TestTerminalRepaintNudgeSkippedForReadOnlyViewer pins the lease requirement:
// a remote-exposed viewer with NO granted writer lease has its resize frame
// dropped at the §4.β boundary, so it issues no resize at all — and certainly
// never a nudge (WriterLease.Resize would return ErrNotWriter anyway).
func TestTerminalRepaintNudgeSkippedForReadOnlyViewer(t *testing.T) {
	w := newRepaintRecordingWriter()
	lm := newRepaintLaunchManager(w, repaintGeometry)
	t.Cleanup(func() { close(lm.sub.release) })
	// remoteWriter is nil on the inner recording manager ⇒ an acquire would be
	// denied; this client stays a pure viewer.
	ts := remoteExposedWSServer(t, newLaunchTestServer(t, lm))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dialRepaintWS(t, ctx, ts)
	defer func() { _ = c.CloseNow() }()

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":40,"cols":120}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if got := w.seq(); len(got) != 0 {
		t.Fatalf("read-only viewer drove the PTY: resize sequence = %v, want none", got)
	}
	if n := lm.localCalls.Load(); n != 0 {
		t.Fatalf("remote-exposed viewer took the owner-local writer %d times", n)
	}
}

// TestTerminalRepaintNudgeSkippedWhenGeometryUnknown pins the all-zero
// geometry guard: with no live snapshot the manager reports "size not yet
// known", so the client's resize is forwarded once and nothing is bounced.
func TestTerminalRepaintNudgeSkippedWhenGeometryUnknown(t *testing.T) {
	w := newRepaintRecordingWriter()
	lm := newRepaintLaunchManager(w, nil) // no snapshot ⇒ ptySizeForHandle is all zero
	t.Cleanup(func() { close(lm.sub.release) })
	ts := httptest.NewServer(newLaunchTestServer(t, lm).Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dialRepaintWS(t, ctx, ts)
	defer func() { _ = c.CloseNow() }()

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":40,"cols":120}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	got := waitResizeSeq(t, w, 1)
	want := [][2]uint16{{40, 120}}
	if !sameSeq(got, want) {
		t.Fatalf("resize sequence = %v, want %v (no nudge on unknown geometry)", got, want)
	}
}

// TestTerminalRepaintNudgeSkippedWhenResizeAlreadyDiffers pins the redundancy
// guard: a resize that actually CHANGES the winsize raises SIGWINCH on its own,
// so no extra bounce is issued.
func TestTerminalRepaintNudgeSkippedWhenResizeAlreadyDiffers(t *testing.T) {
	w := newRepaintRecordingWriter()
	lm := newRepaintLaunchManager(w, repaintGeometry) // live PTY is 40x120
	t.Cleanup(func() { close(lm.sub.release) })
	ts := httptest.NewServer(newLaunchTestServer(t, lm).Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dialRepaintWS(t, ctx, ts)
	defer func() { _ = c.CloseNow() }()

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"resize","rows":30,"cols":100}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	got := waitResizeSeq(t, w, 1)
	want := [][2]uint16{{30, 100}}
	if !sameSeq(got, want) {
		t.Fatalf("resize sequence = %v, want %v (a real size change needs no nudge)", got, want)
	}
}
