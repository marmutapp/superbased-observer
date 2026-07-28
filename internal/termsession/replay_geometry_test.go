package termsession

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// drainPump blocks until the pump has drained at least `want` absolute bytes
// into the session's ring. fakePTY.emit returns once the io.Pipe hand-off
// completes, which is a moment BEFORE outBuf.write lands, so every assertion on
// ring state has to wait for the ring itself — not for emit to return.
func drainPump(t *testing.T, s *Session, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.out.currentTotal() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pump never drained %d bytes (ring total = %d)", want, s.out.currentTotal())
}

// replayNow drains everything a fresh subscriber would replay at attach time,
// stopping at the first caught-up read so it never blocks on the live tail.
func replayNow(t *testing.T, m *Manager, tok string) string {
	t.Helper()
	sub, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer m.Unsubscribe(sub)
	var got bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, wait, closed, lost := sub.s.out.read(&sub.off, buf)
		got.Write(buf[:n])
		sub.lost.Add(lost)
		if n == 0 && (closed || wait != nil) {
			return got.String()
		}
	}
}

// TestReplayStartsAtGeometryBoundary is THE REPRO for the corrupted-reconnect
// defect: output emitted at one PTY width, replayed verbatim into a terminal of
// a DIFFERENT width, renders garbled (each line's first character lands at the
// end of the previous line) — confirmed from a live phone screenshot. It does
// not self-heal, because the reconnecting client's resize is normally a no-op
// (the PTY is already that size, and the kernel skips SIGWINCH on an unchanged
// winsize), so nothing ever repaints.
//
// Against the OLD behaviour (Subscribe starting at s.out.currentBase()) this
// test FAILS: the replay contains the pre-resize "WIDE" bytes. With the
// geometry boundary it passes — a new subscriber only ever replays bytes that
// were emitted at the CURRENT width.
func TestReplayStartsAtGeometryBoundary(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, err := m.Create(sizedSpec(40, 152)) // desktop browser width
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f := sp.last()
	sess := m.get(tok)

	client := attachLocal(t, m, tok)
	defer client.detach(m)

	const wide = "WIDE-LINE-EMITTED-AT-152-COLS\r\n"
	f.emit([]byte(wide))
	drainPump(t, sess, int64(len(wide)))

	if err := client.Resize(30, 47); err != nil { // reconnect from a phone
		t.Fatalf("Resize: %v", err)
	}

	const narrow = "narrow-line-at-47\r\n"
	f.emit([]byte(narrow))
	drainPump(t, sess, int64(len(wide)+len(narrow)))

	got := replayNow(t, m, tok)
	if strings.Contains(got, "WIDE") {
		t.Errorf("replay contains PRE-resize output laid out at the old width:\n%q", got)
	}
	if !strings.Contains(got, "narrow-line-at-47") {
		t.Errorf("replay dropped POST-resize output; got %q", got)
	}
}

// TestReplayBoundaryOnlyMovesOnRealChange is the table-driven guard on WHICH
// resizes move the replay floor. A same-size resize must not truncate (that
// would silently discard good scrollback), and the first-size adoption of a 0×0
// Spec must not truncate either (there is no earlier-width content to discard).
// Only an actual geometry change does.
func TestReplayBoundaryOnlyMovesOnRealChange(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		// resizes applied AFTER the earlier output was emitted.
		resizes    [][2]uint16
		wantEarlie bool // should the pre-resize bytes still replay?
	}{
		{
			name:       "no resize at all — existing behaviour preserved",
			spec:       sizedSpec(24, 80),
			resizes:    nil,
			wantEarlie: true,
		},
		{
			name:       "same-size resize does NOT truncate replay",
			spec:       sizedSpec(24, 80),
			resizes:    [][2]uint16{{24, 80}},
			wantEarlie: true,
		},
		{
			name:       "repeated same-size resizes still do NOT truncate",
			spec:       sizedSpec(24, 80),
			resizes:    [][2]uint16{{24, 80}, {24, 80}, {24, 80}},
			wantEarlie: true,
		},
		{
			name:       "first-size adoption of a 0x0 Spec does NOT truncate",
			spec:       validSpec(), // Rows/Cols == 0
			resizes:    [][2]uint16{{40, 120}},
			wantEarlie: true,
		},
		{
			name:       "a real change DOES truncate",
			spec:       sizedSpec(24, 80),
			resizes:    [][2]uint16{{30, 47}},
			wantEarlie: false,
		},
		{
			name:       "rows-only change DOES truncate",
			spec:       sizedSpec(24, 80),
			resizes:    [][2]uint16{{30, 80}},
			wantEarlie: false,
		},
		{
			name:       "adoption then a real change truncates at the CHANGE",
			spec:       validSpec(),
			resizes:    [][2]uint16{{40, 120}, {30, 47}},
			wantEarlie: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &fakeSpawner{}
			m := newTestManager(t, sp, time.Now)
			tok, err := m.Create(tc.spec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			f := sp.last()
			sess := m.get(tok)

			client := attachLocal(t, m, tok)
			defer client.detach(m)

			const earlier = "EARLIER-OUTPUT\r\n"
			f.emit([]byte(earlier))
			drainPump(t, sess, int64(len(earlier)))

			for _, r := range tc.resizes {
				if err := client.Resize(r[0], r[1]); err != nil {
					t.Fatalf("Resize(%d,%d): %v", r[0], r[1], err)
				}
			}

			const later = "later-output\r\n"
			f.emit([]byte(later))
			drainPump(t, sess, int64(len(earlier)+len(later)))

			got := replayNow(t, m, tok)
			if has := strings.Contains(got, "EARLIER-OUTPUT"); has != tc.wantEarlie {
				t.Errorf("replay contains earlier output = %v, want %v; replay = %q", has, tc.wantEarlie, got)
			}
			if !strings.Contains(got, "later-output") {
				t.Errorf("replay dropped the post-resize output; got %q", got)
			}
		})
	}
}

// TestReplayBoundaryClampsToRingBase covers the ring-overflow interaction: once
// the drop-oldest ring has trimmed PAST a geometry boundary, the cursor must
// clamp forward to currentBase() and never point at bytes that were already
// dropped (which would register as a bogus Lost gap) nor move backwards.
func TestReplayBoundaryClampsToRingBase(t *testing.T) {
	const ring = 4096
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, RingBytes: ring, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, err := m.Create(sizedSpec(40, 152))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f := sp.last()
	sess := m.get(tok)

	client := attachLocal(t, m, tok)
	defer client.detach(m)

	// Resize FIRST so the boundary is planted early, then lap the ring many
	// times over so base races far past it.
	if err := client.Resize(30, 47); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	boundary := sess.geomOff.Load()

	payload := bytes.Repeat([]byte("B"), 1024)
	const produced = 64 * 1024
	for i := 0; i < produced/len(payload); i++ {
		f.emit(payload)
	}
	drainPump(t, sess, int64(produced))

	base := sess.out.currentBase()
	if base <= boundary {
		t.Fatalf("precondition: ring base %d did not lap the boundary %d", base, boundary)
	}
	start := sess.replayStart()
	if start != base {
		t.Fatalf("replayStart = %d, want it clamped to the ring base %d", start, base)
	}

	sub, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer m.Unsubscribe(sub)
	if sub.off < base {
		t.Fatalf("subscriber cursor %d is BEHIND the ring base %d", sub.off, base)
	}
	if sub.off > sess.out.currentTotal() {
		t.Fatalf("subscriber cursor %d is past the ring total %d", sub.off, sess.out.currentTotal())
	}
	if got := sub.Lost(); got != 0 {
		t.Errorf("fresh subscriber Lost = %d, want 0 (the cursor must start inside the ring)", got)
	}
}

// TestReplayBoundaryMonotonic pins that the floor only ever moves FORWARD across
// a run of alternating resizes, so a later attach can never rewind into
// already-superseded geometry.
func TestReplayBoundaryMonotonic(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, err := m.Create(sizedSpec(24, 80))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f := sp.last()
	sess := m.get(tok)

	client := attachLocal(t, m, tok)
	defer client.detach(m)

	prev := sess.replayStart()
	total := 0
	for i, dims := range [][2]uint16{{30, 47}, {30, 47}, {24, 80}, {40, 152}, {40, 152}} {
		chunk := []byte("chunk\r\n")
		f.emit(chunk)
		total += len(chunk)
		drainPump(t, sess, int64(total))
		if err := client.Resize(dims[0], dims[1]); err != nil {
			t.Fatalf("Resize #%d: %v", i, err)
		}
		got := sess.replayStart()
		if got < prev {
			t.Fatalf("replayStart went BACKWARDS after resize #%d: %d -> %d", i, prev, got)
		}
		prev = got
	}
}

// TestReplayStartRaceSafe exercises the boundary read concurrently with resizes
// and pump writes so the atomic + dims-lock pairing is proven race-free
// (meaningful under -race; harmless otherwise).
func TestReplayStartRaceSafe(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, err := m.Create(sizedSpec(10, 10))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f := sp.last()
	sess := m.get(tok)

	client := attachLocal(t, m, tok)
	defer client.detach(m)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for j := uint16(1); j <= 300; j++ {
			_ = client.Resize(j, j)
		}
	}()
	emit := make(chan struct{})
	go func() {
		defer close(emit)
		for j := 0; j < 300; j++ {
			f.emit([]byte("x"))
		}
	}()
	for j := 0; j < 300; j++ {
		if got := sess.replayStart(); got < 0 {
			t.Errorf("replayStart = %d, want a non-negative offset", got)
			break
		}
	}
	<-done
	<-emit
}
