package termsession

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termlease"
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

// localClient is a test helper that mirrors the owner-local /ws/launch path: it
// subscribes a viewer AND acquires the local writer lease, so a test can read
// AND write through one handle as the old single-attach model did.
type localClient struct {
	sub *Subscription
	w   *WriterLease
}

func (c *localClient) Read(p []byte) (int, error)     { return c.sub.Read(p) }
func (c *localClient) Write(p []byte) (int, error)    { return c.w.Write(p) }
func (c *localClient) Resize(rows, cols uint16) error { return c.w.Resize(rows, cols) }

func attachLocal(t *testing.T, m *Manager, tok string) *localClient {
	t.Helper()
	sub, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	w, err := m.AcquireWriterLocal(tok)
	if err != nil {
		t.Fatalf("AcquireWriterLocal: %v", err)
	}
	return &localClient{sub: sub, w: w}
}

func (c *localClient) detach(m *Manager) {
	m.Unsubscribe(c.sub)
	c.w.Release()
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
		{BinPath: "obs", Subcommand: "claude"}, // handoff with no SessionID
	} {
		if _, err := m.Create(bad); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("Create(%+v) err = %v, want ErrInvalidSpec", bad, err)
		}
	}
	// A fresh launch needs no SessionID.
	if _, err := m.Create(Spec{BinPath: "obs", Subcommand: "claude", Fresh: true}); err != nil {
		t.Errorf("fresh Create err = %v, want nil", err)
	}
}

func TestFreshSpecArgv(t *testing.T) {
	got := Spec{BinPath: "obs", Subcommand: "codex", Fresh: true}.argv()
	want := []string{"obs", "codex"}
	if !equalStr(got, want) {
		t.Fatalf("fresh argv = %v, want %v", got, want)
	}
}

func TestOnOutputTap(t *testing.T) {
	sp := &fakeSpawner{}
	got := make(chan []byte, 4)
	m := NewManager(Options{
		Spawner:      sp,
		ReapInterval: time.Hour,
		Now:          time.Now,
		OnOutput: func(_ string, p []byte) {
			b := make([]byte, len(p))
			copy(b, p)
			got <- b
		},
	})
	t.Cleanup(m.Shutdown)
	if _, err := m.Create(validSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.last().emit([]byte("\x1b]133;A\x07hello"))
	select {
	case b := <-got:
		if len(b) == 0 {
			t.Fatal("empty output chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnOutput never fired")
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

	c := attachLocal(t, m, tok)
	// A second viewer is now ALLOWED (the output fan-out, §4.α.1) — no longer
	// rejected. The writer lease stays exclusive to the first (local) client.
	sub2, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("second Subscribe should succeed (multi-viewer): %v", err)
	}
	if got := m.SubscriberCount(tok); got != 2 {
		t.Errorf("SubscriberCount = %d, want 2", got)
	}

	// Keystroke round-trips through the fake echo (viewer sees it too).
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "hi" {
		t.Errorf("round-trip = %q, want %q", buf, "hi")
	}

	// Resize reaches the PTY through the writer lease.
	if err := c.Resize(40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	f := sp.last()
	f.mu.Lock()
	gotR, gotC := f.lastRows, f.lastCols
	f.mu.Unlock()
	if gotR != 40 || gotC != 120 {
		t.Errorf("resize = %dx%d, want 40x120", gotR, gotC)
	}

	m.Unsubscribe(sub2)
	c.detach(m)
	if _, err := m.Subscribe(tok); err != nil {
		t.Errorf("re-Subscribe after unsubscribe: %v", err)
	}
}

// TestViewerCannotWrite proves a read-only subscriber that never acquired the
// writer lease has no way to reach Write/Resize — the write side is only
// reachable through a lease (§4.β side-effect boundary).
func TestViewerCannotWrite(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())

	// A viewer subscribes but never acquires the writer. There is no Write on a
	// Subscription at all; the ONLY input path is a WriterLease. A remote
	// acquire with a zero-value grant is refused (unforgeable-grant gate).
	if _, err := m.Subscribe(tok); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := m.AcquireWriterRemote(tok, termlease.WriterGrant{}); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("AcquireWriterRemote(zero grant) = %v, want ErrNoGrant", err)
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

// TestReplayOnReattach proves the reconnect path: a fresh Subscribe replays the
// ring from the oldest buffered byte, so a client that reconnects re-sees the
// earlier output (the tab-close/refresh survival guarantee).
func TestReplayOnReattach(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	sub, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go f.emit([]byte("hello"))
	if got := readN(t, sub, 5); got != "hello" {
		t.Fatalf("first read = %q, want hello", got)
	}
	m.Unsubscribe(sub)

	// Reconnect: the ring replays from the oldest buffered byte.
	sub2, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("re-Subscribe: %v", err)
	}
	if got := readN(t, sub2, 5); got != "hello" {
		t.Fatalf("replay read = %q, want hello", got)
	}
	m.Unsubscribe(sub2)
}

// TestUnsubscribeDoesNotKill proves Unsubscribe unblocks a waiting Read
// (errUnsubscribed) but keeps the child alive and re-subscribable.
func TestUnsubscribeDoesNotKill(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	sub, _ := m.Subscribe(tok)
	readErr := make(chan error, 1)
	go func() { _, err := sub.Read(make([]byte, 8)); readErr <- err }()
	m.Unsubscribe(sub)
	select {
	case err := <-readErr:
		if !errors.Is(err, errUnsubscribed) {
			t.Fatalf("Read after Unsubscribe = %v, want errUnsubscribed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock on Unsubscribe")
	}
	select {
	case <-f.killed:
		t.Fatal("Unsubscribe killed the PTY")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := m.Subscribe(tok); err != nil {
		t.Fatalf("re-Subscribe after Unsubscribe: %v", err)
	}
}

// TestPumpDrainsWhileDetached proves the always-on pump drains the PTY with no
// client attached (so the child never blocks), and the drained output is
// replayable on a later subscribe.
func TestPumpDrainsWhileDetached(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	emitted := make(chan struct{})
	go func() { f.emit([]byte("bg")); close(emitted) }()
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not drain a detached PTY (emit blocked)")
	}

	sub, _ := m.Subscribe(tok)
	if got := readN(t, sub, 2); got != "bg" {
		t.Fatalf("replay = %q, want bg", got)
	}
	m.Unsubscribe(sub)
}

func TestProcessExitClosesDone(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	sub, _ := m.Subscribe(tok)

	sp.last().exit(7)

	select {
	case <-sub.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after process exit")
	}
	exited, code := sub.Exited()
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

// TestMaxSubscribers proves the per-session viewer fan-out is bounded.
func TestMaxSubscribers(t *testing.T) {
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, MaxSubscribers: 2})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	if _, err := m.Subscribe(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Subscribe(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Subscribe(tok); !errors.Is(err, ErrTooManySubscribers) {
		t.Errorf("3rd Subscribe = %v, want ErrTooManySubscribers", err)
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
	if _, err := m.Subscribe(tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("reaped session still subscribable: %v", err)
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
	sub, _ := m.Subscribe(tok)
	<-sub.Done()

	// Still within linger: not reaped.
	nowP.set(base.Add(10 * time.Second))
	m.reapOnce(nowP.get())
	if _, err := m.Subscribe(tok); errors.Is(err, ErrNotFound) {
		t.Fatal("session reaped inside exit-linger window")
	}

	// Past linger: reaped.
	nowP.set(base.Add(31 * time.Second))
	m.reapOnce(nowP.get())
	if _, err := m.Subscribe(tok); !errors.Is(err, ErrNotFound) {
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
	if _, err := m.Subscribe(tok1); !errors.Is(err, ErrNotFound) {
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
