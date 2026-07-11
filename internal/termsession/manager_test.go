package termsession

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakePTY is an in-memory PTY: writes to it echo back on reads (via an
// io.Pipe), and exit is triggered explicitly by the test. It records
// resize + kill so lifecycle assertions don't need a real process.
type fakePTY struct {
	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter

	exitCh   chan int
	killed   chan struct{}
	killOnce sync.Once

	mu        sync.Mutex
	lastRows  uint16
	lastCols  uint16
	resizeCnt int
}

func newFakePTY() *fakePTY {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakePTY{
		inR: inR, inW: inW, outR: outR, outW: outW,
		exitCh: make(chan int, 1),
		killed: make(chan struct{}),
	}
}

// Read returns terminal output the fake produced (test writes via emit).
func (f *fakePTY) Read(b []byte) (int, error) { return f.outR.Read(b) }

// Write feeds keystrokes; the fake echoes them straight to its output so a
// round-trip test can observe them on Read.
func (f *fakePTY) Write(b []byte) (int, error) {
	go func() { _, _ = f.outW.Write(b) }()
	return len(b), nil
}

// emit injects terminal output independent of a keystroke echo (simulates the
// child writing to the PTY). It blocks on the io.Pipe until the pump drains
// it, so a test that emits while detached proves the always-on pump is running.
func (f *fakePTY) emit(b []byte) { _, _ = f.outW.Write(b) }

func (f *fakePTY) Resize(rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRows, f.lastCols = rows, cols
	f.resizeCnt++
	return nil
}

func (f *fakePTY) Wait() (int, error) {
	select {
	case code := <-f.exitCh:
		return code, nil
	case <-f.killed:
		return -1, nil
	}
}

func (f *fakePTY) Kill() error {
	f.killOnce.Do(func() {
		close(f.killed)
		_ = f.outW.Close()
		_ = f.inW.Close()
	})
	return nil
}

func (f *fakePTY) Close() error { return nil }

// exit makes Wait return code.
func (f *fakePTY) exit(code int) { f.exitCh <- code }

// fakeSpawner hands out pre-built fakePTYs and records the specs it saw.
type fakeSpawner struct {
	mu    sync.Mutex
	ptys  []*fakePTY
	specs []Spec
	err   error
}

func (s *fakeSpawner) Spawn(spec Spec) (PTY, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	f := newFakePTY()
	s.ptys = append(s.ptys, f)
	s.specs = append(s.specs, spec)
	return f, nil
}

func (s *fakeSpawner) last() *fakePTY {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ptys[len(s.ptys)-1]
}

func validSpec() Spec {
	return Spec{BinPath: "/usr/local/bin/observer", Subcommand: "claude", SessionID: "abc123"}
}

func newTestManager(t *testing.T, sp Spawner, now func() time.Time) *Manager {
	t.Helper()
	m := NewManager(Options{
		Spawner:      sp,
		ReapInterval: time.Hour, // the background reaper never fires mid-test
		IdleTimeout:  30 * time.Minute,
		ExitLinger:   30 * time.Second,
		Now:          now,
	})
	t.Cleanup(m.Shutdown)
	return m
}

func TestSpecArgv(t *testing.T) {
	got := Spec{BinPath: "obs", Subcommand: "codex", SessionID: "s1"}.argv()
	want := []string{"obs", "codex", "--continue-from", "s1"}
	if !equalStr(got, want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	got = Spec{BinPath: "obs", Subcommand: "codex", SessionID: "s1", Carry: "full"}.argv()
	want = []string{"obs", "codex", "--continue-from", "s1", "--carry", "full"}
	if !equalStr(got, want) {
		t.Fatalf("argv w/ carry = %v, want %v", got, want)
	}
	got = Spec{BinPath: "obs", Subcommand: "codex", SessionID: "s1", Carry: "full", FromMessage: 12}.argv()
	want = []string{"obs", "codex", "--continue-from", "s1", "--carry", "full", "--from-message", "12"}
	if !equalStr(got, want) {
		t.Fatalf("argv w/ fork = %v, want %v", got, want)
	}
}

func TestCreateValidatesSpec(t *testing.T) {
	m := newTestManager(t, &fakeSpawner{}, time.Now)
	for _, bad := range []Spec{
		{Subcommand: "claude", SessionID: "x"}, // no BinPath
		{BinPath: "obs", SessionID: "x"},       // no Subcommand
		{BinPath: "obs", Subcommand: "claude"}, // no SessionID
	} {
		if _, err := m.Create(bad); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("Create(%+v) err = %v, want ErrInvalidSpec", bad, err)
		}
	}
}

func TestCreateAndRoundTrip(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)

	tok, err := m.Create(validSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok == "" {
		t.Fatal("empty session handle")
	}

	s, err := m.Attach(tok)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// A second attach is rejected while the first holds the claim.
	if _, err := m.Attach(tok); !errors.Is(err, ErrAlreadyAttached) {
		t.Errorf("second Attach err = %v, want ErrAlreadyAttached", err)
	}

	// Keystroke round-trips through the fake echo.
	if _, err := s.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "hi" {
		t.Errorf("round-trip = %q, want %q", buf, "hi")
	}

	// Resize reaches the PTY.
	if err := m.Resize(tok, 40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	f := sp.last()
	f.mu.Lock()
	gotR, gotC := f.lastRows, f.lastCols
	f.mu.Unlock()
	if gotR != 40 || gotC != 120 {
		t.Errorf("resize = %dx%d, want 40x120", gotR, gotC)
	}

	m.Detach(tok)
	if _, err := m.Attach(tok); err != nil {
		t.Errorf("re-Attach after Detach: %v", err)
	}
}

// readN reads exactly n bytes from s, failing if they don't arrive promptly.
func readN(t *testing.T, s io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(s, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readN(%d): %v", n, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("readN(%d): timed out", n)
	}
	return string(buf)
}

// TestReplayOnReattach proves the reconnect path: a fresh Attach replays the
// ring from the start, so a client that reconnects after Detach re-sees the
// earlier output (the tab-close/refresh survival guarantee).
func TestReplayOnReattach(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	s, err := m.Attach(tok)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	go f.emit([]byte("hello"))
	if got := readN(t, s, 5); got != "hello" {
		t.Fatalf("first read = %q, want hello", got)
	}
	m.Detach(tok)

	// Reconnect: the ring replays from the start.
	s2, err := m.Attach(tok)
	if err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	if got := readN(t, s2, 5); got != "hello" {
		t.Fatalf("replay read = %q, want hello", got)
	}
	m.Detach(tok)
}

// TestDetachDoesNotKill proves Detach unblocks a waiting Read (errDetached)
// but keeps the child alive and re-attachable — the core Tier 2 behavior.
func TestDetachDoesNotKill(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	s, _ := m.Attach(tok)
	readErr := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 8)); readErr <- err }()
	m.Detach(tok)
	select {
	case err := <-readErr:
		if !errors.Is(err, errDetached) {
			t.Fatalf("Read after Detach = %v, want errDetached", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock on Detach")
	}
	// The PTY must NOT be killed, and the session stays attachable.
	select {
	case <-f.killed:
		t.Fatal("Detach killed the PTY")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := m.Attach(tok); err != nil {
		t.Fatalf("re-Attach after Detach: %v", err)
	}
	m.Detach(tok)
}

// TestPumpDrainsWhileDetached proves the always-on pump drains the PTY with no
// client attached (so the child never blocks), and the drained output is
// replayable on a later attach.
func TestPumpDrainsWhileDetached(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	// No client attached: the pump must still drain, so this emit (an io.Pipe
	// write) completes instead of blocking forever.
	emitted := make(chan struct{})
	go func() { f.emit([]byte("bg")); close(emitted) }()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not drain a detached PTY (emit blocked)")
	}

	// The drained output is replayable on a later attach.
	s, _ := m.Attach(tok)
	if got := readN(t, s, 2); got != "bg" {
		t.Fatalf("replay = %q, want bg", got)
	}
	m.Detach(tok)
}

func TestProcessExitClosesDone(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	s, _ := m.Attach(tok)

	sp.last().exit(7)

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after process exit")
	}
	exited, code := s.Exited()
	if !exited || code != 7 {
		t.Errorf("Exited() = (%v,%d), want (true,7)", exited, code)
	}
}

func TestMaxConcurrent(t *testing.T) {
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, MaxConcurrent: 2, ReapInterval: time.Hour})
	t.Cleanup(m.Shutdown)
	if _, err := m.Create(validSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(validSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(validSpec()); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("3rd Create err = %v, want ErrTooManySessions", err)
	}
}

func TestIdleReaper(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowP := &atomicTime{}
	nowP.set(base)
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:      sp,
		ReapInterval: time.Hour,
		IdleTimeout:  10 * time.Minute,
		Now:          nowP.get,
	})
	t.Cleanup(m.Shutdown)

	tok, _ := m.Create(validSpec())
	f := sp.last()

	// Advance past the idle timeout; the reaper kills the session.
	nowP.set(base.Add(11 * time.Minute))
	m.reapOnce(nowP.get())

	select {
	case <-f.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle session was not killed")
	}
	if _, err := m.Attach(tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("reaped session still attachable: %v", err)
	}
}

func TestExitLingerThenReap(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowP := &atomicTime{}
	nowP.set(base)
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:      sp,
		ReapInterval: time.Hour,
		ExitLinger:   30 * time.Second,
		Now:          nowP.get,
	})
	t.Cleanup(m.Shutdown)

	tok, _ := m.Create(validSpec())
	f := sp.last()
	f.exit(0)
	// Wait for markDone to land.
	s, _ := m.Attach(tok)
	<-s.Done()

	// Still within linger: not reaped.
	nowP.set(base.Add(10 * time.Second))
	m.reapOnce(nowP.get())
	if _, err := m.Attach(tok); errors.Is(err, ErrNotFound) {
		t.Fatal("session reaped inside exit-linger window")
	}
	m.Detach(tok)

	// Past linger: reaped.
	nowP.set(base.Add(31 * time.Second))
	m.reapOnce(nowP.get())
	if _, err := m.Attach(tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("exited session not reaped past linger: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()
	m.Close(tok)
	m.Close(tok) // must not panic / double-close
	select {
	case <-f.killed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not kill the PTY")
	}
}

func TestShutdownKillsAll(t *testing.T) {
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour})
	tok1, _ := m.Create(validSpec())
	tok2, _ := m.Create(validSpec())
	ptys := append([]*fakePTY(nil), sp.ptys...)
	m.Shutdown()
	for _, f := range ptys {
		select {
		case <-f.killed:
		case <-time.After(2 * time.Second):
			t.Fatal("Shutdown left a live PTY")
		}
	}
	if _, err := m.Attach(tok1); !errors.Is(err, ErrNotFound) {
		t.Error("session survived Shutdown")
	}
	_ = tok2
}

func TestSpawnErrorPropagates(t *testing.T) {
	sp := &fakeSpawner{err: ErrPlatformUnsupported}
	m := newTestManager(t, sp, time.Now)
	if _, err := m.Create(validSpec()); !errors.Is(err, ErrPlatformUnsupported) {
		t.Errorf("Create err = %v, want ErrPlatformUnsupported", err)
	}
}

// --- helpers ---

func equalStr(a, b []string) bool {
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

// atomicTime is a tiny mutable clock for the reaper tests.
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }
