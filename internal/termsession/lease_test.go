package termsession

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termlease"
)

// --- grant helpers: mint a real (unforgeable) WriterGrant via termlease.Authorize ---

type okSess struct{}

func (okSess) Validate(string) error { return nil }

type okPolicy struct{}

func (okPolicy) Allowed(string) bool { return true }

type okCaps struct{}

func (okCaps) ConsumeTerminalControl(_, _, _, _ string) bool { return true }

// remoteGrant mints a valid handle-bound WriterGrant for a device session, the
// same way the cmd adapter will in Commit 3.
func remoteGrant(t *testing.T, handle, device string) termlease.WriterGrant {
	t.Helper()
	g, err := termlease.Authorize(termlease.AuthorizeRequest{
		Handle:          handle,
		DeviceSessionID: device,
		CapabilityToken: "t" + "ok",
		Confirm:         "cn",
		RemoteExposed:   true,
		AllowTerminal:   true,
	}, okSess{}, okPolicy{}, okCaps{})
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	return g
}

// TestSetupSessionRefusesRemoteWriter proves a SpecSetup session (e.g. the
// one-time Tailscale operator grant) is local-writer-only at the lease seam: a
// remote acquire is refused with ErrSetupSessionLocalOnly even with a VALID
// handle-bound grant, while a local acquire still succeeds. This is the codex
// 2026-07-13 crux — CapabilityLocal on the creating POST is not sufficient; the
// pin is on the session kind, enforced here.
func TestSetupSessionRefusesRemoteWriter(t *testing.T) {
	// The pin is on the SESSION KIND (SpecSetup), not the argv content, so
	// EVERY guided-setup spawn site is covered by construction. Enumerate all
	// three shipped setup commands (operator-grant + login + install) so the
	// coverage is explicit for an adversarial reviewer: a paired remote
	// principal can never acquire the writer for any of them.
	setups := map[string][]string{
		"operator-grant": {"sudo", "tailscale", "set", "--operator=alice"},
		"login":          {"sudo", "tailscale", "up"},
		"install":        {"sudo", "sh", "-c", "curl -fsSL https://tailscale.com/install.sh | sh"},
	}
	for name, argv := range setups {
		t.Run(name, func(t *testing.T) {
			sp := &fakeSpawner{}
			m := newTestManager(t, sp, time.Now)
			tok, err := m.Create(Spec{Kind: SpecSetup, SetupArgv: argv})
			if err != nil {
				t.Fatalf("create setup session: %v", err)
			}

			if _, rerr := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x")); !errors.Is(rerr, ErrSetupSessionLocalOnly) {
				t.Fatalf("remote acquire on setup session: got %v, want ErrSetupSessionLocalOnly", rerr)
			}

			l, lerr := m.AcquireWriterLocal(tok)
			if lerr != nil {
				t.Fatalf("local acquire on setup session must succeed, got %v", lerr)
			}
			if !l.IsLocal() {
				t.Fatalf("setup writer must be the local lease")
			}
		})
	}
}

// TestSetupSpecValidation proves Create requires a non-empty SetupArgv for a
// SpecSetup session and never demands BinPath/Subcommand for it.
func TestSetupSpecValidation(t *testing.T) {
	m := newTestManager(t, &fakeSpawner{}, time.Now)
	if _, err := m.Create(Spec{Kind: SpecSetup}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("empty SetupArgv: got %v, want ErrInvalidSpec", err)
	}
	if _, err := m.Create(Spec{Kind: SpecSetup, SetupArgv: []string{""}}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("blank argv[0]: got %v, want ErrInvalidSpec", err)
	}
	if _, err := m.Create(Spec{Kind: SpecSetup, SetupArgv: []string{"true"}}); err != nil {
		t.Fatalf("valid setup spec (no BinPath/Subcommand) must create, got %v", err)
	}
}

// TestConcurrentSetupCreateSingleFlight proves the setup single-flight: N
// concurrent Create calls of the same SetupLabel spawn EXACTLY ONE privileged
// PTY. The winner registers a session; concurrent callers either reuse its
// handle (idempotent) or fail ErrSetupInFlight — never a second spawn.
func TestConcurrentSetupCreateSingleFlight(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	const n = 8
	var wg sync.WaitGroup
	handles := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			handles[i], errs[i] = m.Create(Spec{
				Kind:       SpecSetup,
				SetupArgv:  []string{"true"},
				SetupLabel: "tailscale-login",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	sp.mu.Lock()
	spawned := len(sp.ptys)
	sp.mu.Unlock()
	if spawned != 1 {
		t.Fatalf("setup single-flight: spawned %d privileged PTYs for one label, want exactly 1", spawned)
	}

	winner := ""
	inflight := 0
	for i := 0; i < n; i++ {
		switch {
		case errs[i] == nil:
			if winner == "" {
				winner = handles[i]
			} else if handles[i] != winner {
				t.Errorf("two distinct setup handles handed out: %q vs %q", winner, handles[i])
			}
		case errors.Is(errs[i], ErrSetupInFlight):
			inflight++
		default:
			t.Errorf("unexpected Create error: %v", errs[i])
		}
	}
	if winner == "" {
		t.Fatal("no concurrent setup Create succeeded")
	}
	if got := len(m.Snapshot()); got != 1 {
		t.Fatalf("live sessions = %d, want 1 (single-flight)", got)
	}
	t.Logf("single-flight: 1 spawn, winner=%s, %d refused in-flight", winner, inflight)
}

// TestConcurrentCreateRespectsCapacity proves the atomic capacity reservation:
// N concurrent Create calls against a cap of K yield EXACTLY K live sessions.
// Before the fix the capacity check unlocked before spawn+register, so all N
// could pass the gate and exceed the cap.
func TestConcurrentCreateRespectsCapacity(t *testing.T) {
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:       sp,
		MaxConcurrent: 3,
		ReapInterval:  time.Hour,
		Now:           time.Now,
	})
	t.Cleanup(m.Shutdown)

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = m.Create(Spec{BinPath: "obs", Subcommand: "claude", ArgvMode: ArgvModeFresh})
		}(i)
	}
	close(start)
	wg.Wait()

	ok, tooMany := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrTooManySessions):
			tooMany++
		default:
			t.Errorf("unexpected Create error: %v", e)
		}
	}
	if ok != 3 {
		t.Fatalf("atomic capacity: %d concurrent Creates succeeded, want exactly 3 (cap)", ok)
	}
	if tooMany != n-3 {
		t.Errorf("expected %d ErrTooManySessions, got %d", n-3, tooMany)
	}
	if got := len(m.Snapshot()); got != 3 {
		t.Fatalf("live sessions = %d, want 3", got)
	}
}

// panicOnceSpawner panics on its FIRST Spawn (after the caller has reserved a
// concurrency slot) then delegates to an embedded fakeSpawner, so a test can
// assert that a mid-reservation panic frees the slot AND later Creates on the
// same manager still fill the pool to cap.
type panicOnceSpawner struct {
	fake     fakeSpawner
	mu       sync.Mutex
	panicked bool
}

func (s *panicOnceSpawner) Spawn(spec Spec) (PTY, error) {
	s.mu.Lock()
	if !s.panicked {
		s.panicked = true
		s.mu.Unlock()
		panic("boom: spawn crashed mid-reservation")
	}
	s.mu.Unlock()
	return s.fake.Spawn(spec)
}

// TestCreatePanicDoesNotLeakReservation proves the deferred reservation cleanup
// releases the pending concurrency slot even when Spawner.Spawn PANICS after the
// slot is reserved. Before the fix, releaseReservation was called only on the
// explicit error returns, so a panic left `pending` permanently elevated and
// eventually exhausted MaxConcurrent until a daemon restart. After the fix, a
// recovered panic leaves pending at 0 and MaxConcurrent fresh Creates still all
// succeed (the pool is fully restored) while the cap stays enforced.
func TestCreatePanicDoesNotLeakReservation(t *testing.T) {
	const capN = 3
	sp := &panicOnceSpawner{}
	m := NewManager(Options{
		Spawner:       sp,
		MaxConcurrent: capN,
		ReapInterval:  time.Hour,
		Now:           time.Now,
	})
	t.Cleanup(m.Shutdown)

	// The first Create hits the panicking spawner AFTER reserving a slot; it must
	// panic. Recover it and assert the reservation did not leak.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("Create with a panicking spawner did not panic")
			}
		}()
		_, _ = m.Create(Spec{BinPath: "obs", Subcommand: "claude", ArgvMode: ArgvModeFresh})
	}()

	m.mu.Lock()
	pending := m.pending
	m.mu.Unlock()
	if pending != 0 {
		t.Fatalf("panic leaked the pending reservation: got %d, want 0", pending)
	}

	// The concurrency pool is fully available again: cap fresh Creates all
	// succeed (a leaked reservation would have permanently stolen one slot).
	for i := 0; i < capN; i++ {
		if _, err := m.Create(Spec{BinPath: "obs", Subcommand: "claude", ArgvMode: ArgvModeFresh}); err != nil {
			t.Fatalf("Create %d after recovered panic failed: %v", i, err)
		}
	}
	if got := len(m.Snapshot()); got != capN {
		t.Fatalf("live sessions = %d, want %d (cap fully restored)", got, capN)
	}
	// The cap is still enforced (not loosened by an off-by-one): the next Create
	// past cap is refused.
	if _, err := m.Create(Spec{BinPath: "obs", Subcommand: "claude", ArgvMode: ArgvModeFresh}); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("expected ErrTooManySessions past cap, got %v", err)
	}
}

// TestWriterLeaseRemoteExclusivity proves two concurrent remote acquires yield
// exactly one winner; the loser fails closed (one input source ever).
func TestWriterLeaseRemoteExclusivity(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())

	const n = 8
	var wins int32
	var mu sync.Mutex
	var errCount int
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
			mu.Lock()
			if err == nil {
				wins++
			} else if errors.Is(err, ErrWriterHeld) {
				errCount++
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	if errCount != n-1 {
		t.Fatalf("losers failing closed = %d, want %d", errCount, n-1)
	}
}

// TestLocalNeverEvicted proves a remote acquire is refused while the local
// owner holds the writer, and succeeds only after an explicit local yield.
func TestLocalNeverEvicted(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())

	local, err := m.AcquireWriterLocal(tok)
	if err != nil {
		t.Fatalf("AcquireWriterLocal: %v", err)
	}
	if _, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x")); !errors.Is(err, ErrHeldLocally) {
		t.Fatalf("remote acquire while local holds = %v, want ErrHeldLocally", err)
	}
	// Explicit local yield → remote can now acquire.
	local.Release()
	if _, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x")); err != nil {
		t.Fatalf("remote acquire after local yield = %v, want success", err)
	}
}

// TestLocalTakeoverRevokesRemote proves a local acquire while a remote holds
// the writer ALWAYS revokes the remote: its Revoked channel closes and its next
// Write errors (ErrNotWriter) — the remote lost control.
func TestLocalTakeoverRevokesRemote(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())

	remote, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}
	// Local takeover cannot be refused.
	if _, err := m.AcquireWriterLocal(tok); err != nil {
		t.Fatalf("local takeover = %v, want success", err)
	}
	select {
	case <-remote.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("remote lease Revoked channel not closed on local takeover")
	}
	if _, err := remote.Write([]byte("x")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("revoked remote Write = %v, want ErrNotWriter", err)
	}
}

// TestTakeoverVsInflightWriteRace is a SHIP GATE (§4.α.2b): it hammers a remote
// writer's Write concurrently with a local takeover and proves "one input
// source ever" is an invariant, not a race — under -race there is no data race,
// and after the takeover no remote write reaches the PTY (every post-takeover
// remote Write errors with ErrNotWriter).
func TestTakeoverVsInflightWriteRace(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())

	remote, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Remote writer hammering the PTY.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = remote.Write([]byte("r"))
		}
	}()
	// Local takeover mid-flight.
	time.Sleep(2 * time.Millisecond)
	localLease, err := m.AcquireWriterLocal(tok)
	if err != nil {
		t.Fatalf("local takeover: %v", err)
	}
	// After takeover, the remote lease is fenced: every remote Write now errors.
	time.Sleep(2 * time.Millisecond)
	for i := 0; i < 100; i++ {
		if _, werr := remote.Write([]byte("r")); !errors.Is(werr, ErrNotWriter) {
			t.Fatalf("post-takeover remote Write = %v, want ErrNotWriter (one input source ever)", werr)
		}
	}
	// The local lease drives the PTY.
	if _, werr := localLease.Write([]byte("l")); werr != nil {
		t.Fatalf("local write after takeover = %v", werr)
	}
	close(stop)
	wg.Wait()
}

// TestConcurrentSubscriptionDuringPumpWrite is a SHIP GATE (§4.α.1, 2nd-pass
// finding 4): a subscriber joining mid-stream while the pump writes sees a
// coherent, gap-free, in-order suffix of the byte stream (each byte delivered
// exactly once). The ring is the SINGLE authoritative source, so a joiner's
// cursor starts at a consistent watermark (the ring base) and only moves
// forward.
func TestConcurrentSubscriptionDuringPumpWrite(t *testing.T) {
	sp := &fakeSpawner{}
	// Large ring so nothing trims during the test (no legitimate Lost gaps).
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, RingBytes: 1 << 20, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	// A monotonically increasing byte stream: byte i == byte(i%251). A reader
	// that sees each byte exactly once, in order, from some start offset can
	// verify continuity: consecutive bytes differ by +1 (mod 251).
	const total = 60000
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		chunk := make([]byte, 500)
		for i := 0; i < total; i += len(chunk) {
			for j := range chunk {
				chunk[j] = byte((i + j) % 251)
			}
			f.emit(chunk)
		}
	}()

	// Join mid-stream.
	time.Sleep(1 * time.Millisecond)
	sub, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer m.Unsubscribe(sub)

	// Read a run of bytes and assert strict +1 (mod 251) continuity — no dup, no
	// gap, in order. Read until we've verified a healthy sample or the stream
	// ends.
	buf := make([]byte, 4096)
	var last int = -1
	verified := 0
	deadline := time.After(5 * time.Second)
	for verified < 20000 {
		select {
		case <-deadline:
			t.Fatalf("timed out after verifying %d bytes", verified)
		default:
		}
		n, rerr := sub.Read(buf)
		if sub.Lost() != 0 {
			t.Fatalf("unexpected Lost=%d — large ring should not trim", sub.Lost())
		}
		for i := 0; i < n; i++ {
			cur := int(buf[i])
			if last >= 0 {
				want := (last + 1) % 251
				if cur != want {
					t.Fatalf("discontinuity at byte %d: got %d want %d (dup or gap)", verified, cur, want)
				}
			}
			last = cur
			verified++
		}
		if rerr != nil {
			break
		}
	}
	<-writerDone
}

// TestSlowSubscriberDropsOldestNeverStalls proves a slow/stuck viewer that
// overruns the ring is drop-oldest degraded (a growing Lost gap) and NEVER
// back-pressures the pump: a second, fast viewer keeps receiving fresh output.
func TestSlowSubscriberDropsOldestNeverStalls(t *testing.T) {
	sp := &fakeSpawner{}
	// Small ring so a stuck reader overruns quickly.
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, RingBytes: 4096, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	slow, _ := m.Subscribe(tok) // never reads — will fall behind
	fast, _ := m.Subscribe(tok)

	// Pump ~64 KiB through a 4 KiB ring: the slow reader must lose bytes, the
	// fast reader must keep receiving without the pump stalling. The pump drains
	// the PTY into the ring independently of any reader, so emitDone closing
	// proves the pump never back-pressured on the stuck slow viewer.
	emitDone := make(chan struct{})
	go func() {
		defer close(emitDone)
		payload := bytes.Repeat([]byte("A"), 1024)
		for i := 0; i < 64; i++ {
			f.emit(payload)
		}
	}()

	// Fast reader drains continuously while the slow one never reads.
	got := 0
	buf := make([]byte, 8192)
	deadline := time.After(5 * time.Second)
	for got < 4096 {
		select {
		case <-deadline:
			t.Fatalf("fast reader stalled at %d bytes (pump back-pressured by slow viewer?)", got)
		default:
		}
		n, err := fast.Read(buf)
		got += n
		if err != nil {
			break
		}
	}
	if got < 4096 {
		t.Fatalf("fast reader only got %d bytes", got)
	}
	// The pump fully drained all 64 KiB into the 4 KiB ring without stalling on
	// the never-reading slow viewer.
	select {
	case <-emitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pump stalled: emit of 64 KiB did not complete with a stuck slow viewer")
	}
	// The slow reader, once it finally reads, reports a Lost gap (ring overran
	// its cursor). Loop until the pump-drained ring has trimmed past its cursor.
	for slow.Lost() == 0 {
		n, _, _, lost := m.get(tok).out.read(&slow.off, buf)
		slow.lost.Add(lost)
		if lost == 0 && n == 0 {
			break
		}
	}
	if slow.Lost() == 0 {
		t.Fatal("slow subscriber reported no Lost gap despite ring overrun")
	}
}

// TestWriterLeaseIdleExpiry proves the reaper revokes a REMOTE writer lease
// past its idle lifetime (§4.α.2c). The lifetimes are remote-authority
// security bounds — local leases are exempt (see the exemption test below).
func TestWriterLeaseIdleExpiry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowP := &atomicTime{}
	nowP.set(base)
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:         sp,
		ReapInterval:    time.Hour,
		WriterLeaseIdle: 5 * time.Minute,
		WriterLeaseMax:  30 * time.Minute,
		Now:             nowP.get,
	})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())

	l, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}
	nowP.set(base.Add(6 * time.Minute)) // past idle
	m.reapOnce(nowP.get())
	select {
	case <-l.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("idle remote writer lease not revoked by reaper")
	}
	if _, werr := l.Write([]byte("x")); !errors.Is(werr, ErrNotWriter) {
		t.Fatalf("expired-lease Write = %v, want ErrNotWriter", werr)
	}
}

// TestWriterLeaseHardCapExpiry proves the hard-cap revokes even a
// continuously-written REMOTE lease (§4.α.2c).
func TestWriterLeaseHardCapExpiry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowP := &atomicTime{}
	nowP.set(base)
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:         sp,
		ReapInterval:    time.Hour,
		WriterLeaseIdle: 5 * time.Minute,
		WriterLeaseMax:  30 * time.Minute,
		Now:             nowP.get,
	})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	l, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}

	// Keep it "active" but push past the hard cap.
	nowP.set(base.Add(20 * time.Minute))
	_, _ = l.Write([]byte("x")) // refreshes idle clock, not the hard cap
	nowP.set(base.Add(31 * time.Minute))
	m.reapOnce(nowP.get())
	select {
	case <-l.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("hard-capped remote writer lease not revoked")
	}
}

// TestLocalWriterLeaseExemptFromLifetimeSweep pins the continuity half of the
// §4.α.2c scoping: a LOCAL lease (the native wrapper's seat, the loopback
// dashboard) never idle-expires and never hits the hard cap — the operator at
// the keyboard keeps write authority across hours of idle. Idle-expiring it
// silently killed input in long-lived attach sessions.
func TestLocalWriterLeaseExemptFromLifetimeSweep(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	nowP := &atomicTime{}
	nowP.set(base)
	sp := &fakeSpawner{}
	m := NewManager(Options{
		Spawner:         sp,
		ReapInterval:    time.Hour,
		WriterLeaseIdle: 5 * time.Minute,
		WriterLeaseMax:  30 * time.Minute,
		Now:             nowP.get,
	})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	l, err := m.AcquireWriterLocal(tok)
	if err != nil {
		t.Fatalf("AcquireWriterLocal: %v", err)
	}

	// Way past both the idle lifetime and the hard cap.
	nowP.set(base.Add(24 * time.Hour))
	m.reapOnce(nowP.get())
	select {
	case <-l.Revoked():
		t.Fatal("local writer lease was revoked by the lifetime sweep — locals are exempt")
	default:
	}
	if _, werr := l.Write([]byte("x")); werr != nil {
		t.Fatalf("local lease Write after 24h idle = %v, want success", werr)
	}
}

// TestRevokeAllWritersTerminatesLiveWriter proves the manager-level global kill
// (allow_terminal→false / remote disable) terminates a LIVE writer, not just
// future acquires (§4.δ / negative-test #9).
func TestRevokeAllWritersTerminatesLiveWriter(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	remote, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}
	if n := m.RevokeAllWriters("allow_terminal disabled"); n != 1 {
		t.Fatalf("RevokeAllWriters revoked %d, want 1", n)
	}
	select {
	case <-remote.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("live writer not revoked by RevokeAllWriters")
	}
	if _, werr := remote.Write([]byte("x")); !errors.Is(werr, ErrNotWriter) {
		t.Fatalf("post-revoke Write = %v, want ErrNotWriter", werr)
	}
}

// TestRevokeAllRemoteWritersLeavesLocalWriter proves the remote-only global kill
// (remote disable / rotate / allow_terminal→false) terminates every REMOTE
// writer lease while leaving an owner-LOCAL loopback writer completely untouched
// (§8.1 item 8) — the local operator never loses control of their own terminal.
func TestRevokeAllRemoteWritersLeavesLocalWriter(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)

	// Session A: a REMOTE writer.
	remoteTok, _ := m.Create(validSpec())
	remote, err := m.AcquireWriterRemote(remoteTok, remoteGrant(t, remoteTok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}
	// Session B: an owner-LOCAL writer.
	localTok, _ := m.Create(validSpec())
	local, err := m.AcquireWriterLocal(localTok)
	if err != nil {
		t.Fatalf("AcquireWriterLocal: %v", err)
	}

	if n := m.RevokeAllRemoteWriters("remote disabled"); n != 1 {
		t.Fatalf("RevokeAllRemoteWriters revoked %d, want 1 (only the remote lease)", n)
	}
	// The remote lease is dead: channel closed, kind = revoked (NOT takeover), a
	// subsequent Write is fenced out before the PTY.
	select {
	case <-remote.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("remote lease not revoked by RevokeAllRemoteWriters")
	}
	if remote.RevokeIsTakeover() {
		t.Fatal("remote revoke misclassified as a takeover — the bridge would wrongly demote instead of close")
	}
	if _, werr := remote.Write([]byte("x")); !errors.Is(werr, ErrNotWriter) {
		t.Fatalf("post-revoke remote Write = %v, want ErrNotWriter", werr)
	}
	// The LOCAL writer is untouched: its channel is open and it still drives.
	select {
	case <-local.Revoked():
		t.Fatal("owner-local writer was revoked by RevokeAllRemoteWriters — local must be untouched")
	default:
	}
	if _, werr := local.Write([]byte("l")); werr != nil {
		t.Fatalf("local writer must still drive after RevokeAllRemoteWriters, got %v", werr)
	}
	// A second call is a no-op (idempotent, nothing remote left).
	if n := m.RevokeAllRemoteWriters("again"); n != 0 {
		t.Fatalf("second RevokeAllRemoteWriters revoked %d, want 0", n)
	}
}

// TestRevokeRemoteWriterByHolder proves a device-session revoke kills ONLY the
// writer lease keyed on that exact device's FULL session hash (grant.HolderKey()),
// leaving another device's remote writer AND the local writer untouched. It also
// pins the WP-F width change: the 8-char DISPLAY fingerprint (grant.Holder() /
// lease.Holder()) no longer revokes — only the full hash does.
func TestRevokeRemoteWriterByHolder(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)

	tokX, _ := m.Create(validSpec())
	gX := remoteGrant(t, tokX, "device-x")
	rx, err := m.AcquireWriterRemote(tokX, gX)
	if err != nil {
		t.Fatalf("acquire device-x: %v", err)
	}
	tokY, _ := m.Create(validSpec())
	ry, err := m.AcquireWriterRemote(tokY, remoteGrant(t, tokY, "device-y"))
	if err != nil {
		t.Fatalf("acquire device-y: %v", err)
	}

	// The lease keys its holder IDENTITY on the FULL hash (grant.HolderKey(), 64
	// hex chars), while the DISPLAY fingerprint (lease.Holder() / grant.Holder())
	// is the 8-char prefix of it.
	keyX := gX.HolderKey()
	fpX := rx.Holder()
	if keyX == "" || keyX == "device-x" || len(keyX) != 64 {
		t.Fatalf("holder key %q must be the full 64-char session hash, not the raw id", keyX)
	}
	if len(fpX) != 8 || fpX != keyX[:8] {
		t.Fatalf("lease.Holder() %q must be the 8-char display prefix of the full key %q", fpX, keyX)
	}
	// Non-matching / sentinel values revoke nothing.
	if m.RevokeRemoteWriterByHolder("local", "x") {
		t.Fatal(`RevokeRemoteWriterByHolder("local") must never match a remote lease`)
	}
	if m.RevokeRemoteWriterByHolder("deadbeef", "x") {
		t.Fatal("RevokeRemoteWriterByHolder with an unknown key revoked a lease")
	}
	// WP-F width pin: the 8-char DISPLAY fingerprint must NOT revoke — the lease
	// keys on the full hash, so a prefix-only value matches nothing (no mixed-
	// width comparison window).
	if m.RevokeRemoteWriterByHolder(fpX, "prefix") {
		t.Fatal("8-char display fingerprint revoked a full-hash-keyed lease — the width change is not in effect")
	}
	select {
	case <-rx.Revoked():
		t.Fatal("device-x lease was revoked by a prefix-only value — must key on the full hash")
	default:
	}

	// The FULL hash revokes exactly device-x.
	if !m.RevokeRemoteWriterByHolder(keyX, "device session revoked") {
		t.Fatal("RevokeRemoteWriterByHolder(fullHash) did not revoke device-x's lease")
	}
	select {
	case <-rx.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("device-x lease not revoked")
	}
	if _, werr := rx.Write([]byte("x")); !errors.Is(werr, ErrNotWriter) {
		t.Fatalf("post-revoke device-x Write = %v, want ErrNotWriter", werr)
	}
	// device-y is untouched.
	select {
	case <-ry.Revoked():
		t.Fatal("device-y lease was revoked — only the named device must die")
	default:
	}
	if _, werr := ry.Write([]byte("y")); werr != nil {
		t.Fatalf("device-y writer must still drive, got %v", werr)
	}
}

// TestRevokeRemoteWriterByHolderDistinguishesPrefixCollision proves the WP-F
// rekey closes the 8-char-prefix over-revoke: two remote leases whose holder
// keys share the same 8-char display fingerprint but differ in the full hash are
// revoked independently, and the shared 8-char prefix revokes NEITHER. The
// holders are set directly (via the in-package acquireWriter seam) so the test
// can construct a deterministic prefix collision that real sha256 hashes make
// astronomically rare.
func TestRevokeRemoteWriterByHolderDistinguishesPrefixCollision(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)

	const shared = "aaaaaaaa"                // the colliding 8-char display prefix
	keyA := shared + strings.Repeat("0", 56) // 64 hex chars, distinct tail
	keyB := shared + strings.Repeat("1", 56)
	if len(keyA) != 64 || len(keyB) != 64 || keyA == keyB || keyA[:8] != keyB[:8] {
		t.Fatalf("test setup: keys must be 64 chars, distinct, and share an 8-char prefix")
	}

	tokA, _ := m.Create(validSpec())
	sa := m.get(tokA)
	la, err := sa.acquireWriter(termlease.RequesterRemote, keyA, false)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	tokB, _ := m.Create(validSpec())
	sb := m.get(tokB)
	lb, err := sb.acquireWriter(termlease.RequesterRemote, keyB, false)
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}

	// Both display as the same 8-char fingerprint (the collision).
	if la.Holder() != shared || lb.Holder() != shared {
		t.Fatalf("expected both leases to display fingerprint %q, got %q / %q", shared, la.Holder(), lb.Holder())
	}
	// The shared 8-char prefix revokes NEITHER — the match is whole-string.
	if m.RevokeRemoteWriterByHolder(shared, "prefix") {
		t.Fatal("the shared 8-char prefix revoked a full-hash-keyed lease")
	}
	select {
	case <-la.Revoked():
		t.Fatal("A revoked by the shared prefix")
	case <-lb.Revoked():
		t.Fatal("B revoked by the shared prefix")
	default:
	}
	// The full keyA revokes ONLY A, leaving B (same prefix) untouched.
	if !m.RevokeRemoteWriterByHolder(keyA, "revoke A") {
		t.Fatal("full keyA did not revoke A")
	}
	select {
	case <-la.Revoked():
	case <-time.After(2 * time.Second):
		t.Fatal("A not revoked by its full key")
	}
	select {
	case <-lb.Revoked():
		t.Fatal("B was revoked by A's full key — prefix collision over-revoke NOT closed")
	default:
	}
	if _, werr := lb.Write([]byte("b")); werr != nil {
		t.Fatalf("B must still drive after A's revoke, got %v", werr)
	}
}

// TestWriterLocalHolderSentinelUnchanged proves the owner-local sentinel holder
// is preserved end-to-end by the WP-F width change: it stays the literal "local"
// (not hashed, not truncated) for display AND is never matched by a remote
// revoke-by-holder.
func TestWriterLocalHolderSentinelUnchanged(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())
	l, err := m.AcquireWriterLocal(tok)
	if err != nil {
		t.Fatalf("AcquireWriterLocal: %v", err)
	}
	if l.Holder() != "local" {
		t.Fatalf("local lease Holder() = %q, want the unchanged sentinel %q", l.Holder(), "local")
	}
	if !l.IsLocal() {
		t.Fatal("local lease must report IsLocal()")
	}
	if holder, held := m.WriterHolder(tok); !held || holder != "local" {
		t.Fatalf("WriterHolder = (%q,%v), want (\"local\",true)", holder, held)
	}
	// A remote revoke-by-holder must never touch the local writer, whether the
	// passed key is the sentinel itself or any other value.
	if m.RevokeRemoteWriterByHolder("local", "x") {
		t.Fatal(`RevokeRemoteWriterByHolder("local") revoked the owner-local writer`)
	}
	select {
	case <-l.Revoked():
		t.Fatal("owner-local writer lease was revoked by a remote revoke-by-holder")
	default:
	}
	if _, werr := l.Write([]byte("ok")); werr != nil {
		t.Fatalf("local writer must still drive, got %v", werr)
	}
}

// TestLocalTakeoverMarksRevokeKindTakeover proves the incumbent remote lease's
// terminating kind is LeaseTakenOver on a local takeover (so the remote bridge
// DEMOTES), whereas an explicit Revoke marks LeaseRevoked (so the bridge CLOSES).
func TestLocalTakeoverMarksRevokeKindTakeover(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	tok, _ := m.Create(validSpec())

	remote, err := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	if err != nil {
		t.Fatalf("AcquireWriterRemote: %v", err)
	}
	if _, err := m.AcquireWriterLocal(tok); err != nil {
		t.Fatalf("local takeover: %v", err)
	}
	<-remote.Revoked()
	if !remote.RevokeIsTakeover() {
		t.Fatalf("local takeover must mark RevokeKind=LeaseTakenOver, got %q", remote.RevokeKind())
	}

	// Contrast: a fresh remote lease that is explicitly revoked is NOT a takeover.
	tok2, _ := m.Create(validSpec())
	r2, _ := m.AcquireWriterRemote(tok2, remoteGrant(t, tok2, "device-z"))
	r2.Revoke()
	<-r2.Revoked()
	if r2.RevokeIsTakeover() {
		t.Fatalf("explicit Revoke must NOT be a takeover, got kind=%q", r2.RevokeKind())
	}
}

// TestLeaseAuditEvents proves the metadata-only audit tap fires for the lease
// transitions the remote-execute tier records (§8.1 audit list).
func TestLeaseAuditEvents(t *testing.T) {
	sp := &fakeSpawner{}
	var mu sync.Mutex
	var kinds []LeaseEventKind
	m := NewManager(Options{
		Spawner:      sp,
		ReapInterval: time.Hour,
		Now:          time.Now,
		OnLeaseEvent: func(e LeaseEvent) {
			mu.Lock()
			kinds = append(kinds, e.Kind)
			mu.Unlock()
		},
	})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())

	remote, _ := m.AcquireWriterRemote(tok, remoteGrant(t, tok, "device-x"))
	_ = remote
	// Local takeover: emits taken_over + acquired.
	local, _ := m.AcquireWriterLocal(tok)
	local.Release()

	mu.Lock()
	defer mu.Unlock()
	has := func(k LeaseEventKind) bool {
		for _, g := range kinds {
			if g == k {
				return true
			}
		}
		return false
	}
	for _, want := range []LeaseEventKind{LeaseAcquired, LeaseTakenOver, LeaseReleased} {
		if !has(want) {
			t.Errorf("missing audit event %q in %v", want, kinds)
		}
	}
}

// standingGrant mints a grant through the REAL AuthorizeStanding path so its
// provenance (Standing()==true) is the genuine credential-leg fact, never a
// fabricated flag.
func standingGrant(t *testing.T, handle, device string) termlease.WriterGrant {
	t.Helper()
	g, err := termlease.AuthorizeStanding(termlease.AuthorizeRequest{
		Handle:          handle,
		DeviceSessionID: device,
		CapabilityToken: "standing-" + "cred",
		RemoteExposed:   true,
		AllowTerminal:   true,
	}, okSess{}, okPolicy{}, okStandingVerifier{})
	if err != nil {
		t.Fatalf("mint standing grant: %v", err)
	}
	if !g.Standing() {
		t.Fatal("AuthorizeStanding grant must report Standing()==true")
	}
	return g
}

type okStandingVerifier struct{}

func (okStandingVerifier) VerifyStandingTerminalControl(secret, dev, handle string) bool {
	return true
}

// TestLocalTakeoverOfStandingWriterFiresHook pins the provenance hook: a LOCAL
// takeover of a remote writer whose lease was minted through the STANDING
// credential fires SetOnStandingLocalTakeover exactly once (async) with the
// session handle + the superseded holder key; a takeover of a SINGLE-USE
// remote writer never fires it. Policy stays behind the hook — termsession
// only reports provenance.
func TestLocalTakeoverOfStandingWriterFiresHook(t *testing.T) {
	sp := &fakeSpawner{}
	m := newTestManager(t, sp, time.Now)
	fired := make(chan [2]string, 4)
	m.SetOnStandingLocalTakeover(func(handle, revokedHolder string) {
		fired <- [2]string{handle, revokedHolder}
	})

	tok, _ := m.Create(validSpec())
	g := standingGrant(t, tok, "device-standing")
	if _, err := m.AcquireWriterRemote(tok, g); err != nil {
		t.Fatalf("AcquireWriterRemote(standing): %v", err)
	}
	if _, err := m.AcquireWriterLocal(tok); err != nil {
		t.Fatalf("local takeover: %v", err)
	}
	select {
	case got := <-fired:
		if got[0] != tok {
			t.Errorf("hook handle = %q, want %q", got[0], tok)
		}
		if got[1] != g.HolderKey() {
			t.Errorf("hook revokedHolder = %q, want the superseded FULL holder key %q", got[1], g.HolderKey())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("standing-takeover hook did not fire on local takeover of a standing writer")
	}

	// Single-use provenance: same takeover shape, hook must stay silent.
	tok2, _ := m.Create(validSpec())
	if _, err := m.AcquireWriterRemote(tok2, remoteGrant(t, tok2, "device-single")); err != nil {
		t.Fatalf("AcquireWriterRemote(single-use): %v", err)
	}
	if _, err := m.AcquireWriterLocal(tok2); err != nil {
		t.Fatalf("local takeover 2: %v", err)
	}
	select {
	case got := <-fired:
		t.Fatalf("hook fired for a single-use takeover: %v", got)
	case <-time.After(150 * time.Millisecond):
	}
}
