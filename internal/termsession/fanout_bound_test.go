package termsession

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestRemoteTerminalSubscriberLimit proves the per-session viewer fan-out is
// bounded (§4.α.1): the (MaxSubscribers+1)-th viewer is refused with
// ErrTooManySubscribers, and the refusal does NOT disturb the existing viewers
// or the always-on pump — a live viewer keeps receiving fresh output after the
// rejection. (TestMaxSubscribers covers the bare refusal; this pins the
// "existing viewers/pump unaffected" property the §8.1 review requires.)
func TestRemoteTerminalSubscriberLimit(t *testing.T) {
	const limit = 3
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, MaxSubscribers: limit, RingBytes: 1 << 20, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	subs := make([]*Subscription, 0, limit)
	for i := 0; i < limit; i++ {
		sub, err := m.Subscribe(tok)
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		subs = append(subs, sub)
	}
	// The (limit+1)-th viewer is refused.
	if _, err := m.Subscribe(tok); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("Subscribe past limit = %v, want ErrTooManySubscribers", err)
	}
	if got := m.SubscriberCount(tok); got != limit {
		t.Fatalf("SubscriberCount = %d, want %d (refusal must not evict a live viewer)", got, limit)
	}

	// The pump + existing viewers are unaffected: emit fresh output and prove a
	// live viewer still receives it after the rejection.
	go f.emit([]byte("post-rejection output"))
	buf := make([]byte, 64)
	deadline := time.After(3 * time.Second)
	got := 0
	for got == 0 {
		select {
		case <-deadline:
			t.Fatal("live viewer received no output after a subscriber-limit rejection (pump disturbed?)")
		default:
		}
		n, err := subs[0].Read(buf)
		got += n
		if err != nil {
			t.Fatalf("live viewer Read: %v", err)
		}
	}
	for _, s := range subs {
		m.Unsubscribe(s)
	}
	// A freed slot is reusable — the bound is a live count, not a high-water mark.
	if _, err := m.Subscribe(tok); err != nil {
		t.Fatalf("Subscribe after freeing a slot: %v", err)
	}
}

// TestSlowRemoteViewerNeverBackpressuresPTY proves a slow/stalled viewer is
// drop-oldest degraded (a growing Lost gap) while the PTY pump and a healthy
// concurrent viewer keep flowing — the fan-out never stalls the PTY on the
// slowest subscriber (§4.α.1). Run under -race.
func TestSlowRemoteViewerNeverBackpressuresPTY(t *testing.T) {
	sp := &fakeSpawner{}
	// Small ring so a stuck reader overruns quickly.
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, RingBytes: 4096, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	slow, _ := m.Subscribe(tok) // never reads — falls behind
	fast, _ := m.Subscribe(tok)

	// Drain the fast viewer continuously in the background so the pump always has
	// a live consumer, while the slow one stays stuck.
	fastGot := make(chan int, 1)
	go func() {
		buf := make([]byte, 8192)
		total := 0
		for total < 4096 {
			n, err := fast.Read(buf)
			total += n
			if err != nil {
				break
			}
		}
		fastGot <- total
	}()

	// Push ~64 KiB through the 4 KiB ring. emitDone closing proves the pump never
	// back-pressured on the stuck slow viewer.
	emitDone := make(chan struct{})
	go func() {
		defer close(emitDone)
		payload := bytes.Repeat([]byte("A"), 1024)
		for i := 0; i < 64; i++ {
			f.emit(payload)
		}
	}()

	select {
	case <-emitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pump stalled: 64 KiB emit did not complete with a stuck slow viewer")
	}
	select {
	case n := <-fastGot:
		if n < 4096 {
			t.Fatalf("healthy concurrent viewer only received %d bytes", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("healthy concurrent viewer stalled (pump back-pressured by the slow viewer?)")
	}

	// The slow viewer, once it finally reads, reports a nonzero Lost gap — it was
	// drop-oldest degraded, not stalled.
	buf := make([]byte, 8192)
	deadline := time.After(5 * time.Second)
	for slow.Lost() == 0 {
		select {
		case <-deadline:
			t.Fatal("slow viewer never reported a Lost gap despite ring overrun")
		default:
		}
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

// TestRemoteReplayBound proves attach-time replay is bounded by the ring
// (§4.α.1): a subscriber that joins after far more than ring-size bytes were
// produced replays AT MOST ring-size bytes (the older output was trimmed), never
// the unbounded whole stream. The bound is what makes a late remote attach cheap
// and memory-safe.
func TestRemoteReplayBound(t *testing.T) {
	const ring = 4096
	const produced = 64 * 1024
	sp := &fakeSpawner{}
	m := NewManager(Options{Spawner: sp, ReapInterval: time.Hour, RingBytes: ring, Now: time.Now})
	t.Cleanup(m.Shutdown)
	tok, _ := m.Create(validSpec())
	f := sp.last()

	// Produce >>ring bytes with NO subscriber attached, SYNCHRONOUSLY so the
	// stream is fully lapped before anyone attaches (f.emit blocks on the pipe
	// until the always-on pump drains it into the bounded ring, trimming the
	// oldest). Producing before Subscribe keeps attach-time replay separate from
	// any live tail.
	payload := bytes.Repeat([]byte("B"), 1024)
	for i := 0; i < produced/1024; i++ {
		f.emit(payload)
	}
	sess := m.get(tok)
	// Wait for the pump to have drained ALL produced bytes into the ring —
	// not just for trimming to have begun (currentBase() != 0) — so a slow
	// runner's straggling pump appends can't land after Subscribe and inflate
	// the replay count past the ring size.
	deadline := time.After(5 * time.Second)
	for sess.out.currentTotal() != int64(produced) {
		select {
		case <-deadline:
			t.Fatal("pump never drained the full stream; test precondition not met")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	sub, err := m.Subscribe(tok)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer m.Unsubscribe(sub)

	// Drain the replay (everything buffered at attach) WITHOUT blocking: stop at
	// the first caught-up read (n==0, wait!=nil, not closed).
	buf := make([]byte, 8192)
	replayed := int64(0)
	for {
		n, wait, closed, lost := sess.out.read(&sub.off, buf)
		sub.lost.Add(lost)
		replayed += int64(n)
		if n == 0 && (closed || wait != nil) {
			break
		}
		if replayed > int64(ring)+1<<20 {
			t.Fatalf("replay exceeded the ring by a wide margin (%d bytes) — attach replay is UNBOUNDED", replayed)
		}
	}
	if replayed > int64(ring) {
		t.Fatalf("attach replay = %d bytes, want <= ring size %d (bounded replay)", replayed, ring)
	}
	if replayed >= int64(produced) {
		t.Fatalf("attach replayed the whole %d-byte stream — replay is not ring-bounded", produced)
	}
	if replayed == 0 {
		t.Fatal("attach replayed nothing; expected a bounded ring-sized suffix")
	}
}
