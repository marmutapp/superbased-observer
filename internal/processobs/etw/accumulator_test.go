package etw

import (
	"sync"
	"testing"
)

// TestAddIsCumulativeNotPerEvent is THE regression guard for §0.1 of the
// Windows-parity plan — the highest-risk bug in the arc, and the one that fails
// silently.
//
// ETW reports the bytes moved by EACH event. Linux reports a monotonic running
// total per pid, and every consumer differentiates consecutive samples to get a
// rate. Feed the per-event numbers straight through and no error fires, no log
// line appears and no other test fails: the chart just reads low, by a factor
// of the sampling interval.
//
// So: a per-event stream of 100, 250, 90 must be observed as 100, 350, 440.
func TestAddIsCumulativeNotPerEvent(t *testing.T) {
	t.Parallel()

	const pid = 4242
	perEvent := []int64{100, 250, 90}
	wantCumulative := []int64{100, 350, 440}

	a := NewAccumulator(0)
	for i, n := range perEvent {
		a.Add(pid, DirectionReceive, n)

		in, out, ok := a.NetworkBytes(pid)
		if !ok {
			t.Fatalf("after event %d: NetworkBytes reported no counter for pid %d", i+1, pid)
		}
		if out != 0 {
			t.Errorf("after event %d: out = %d, want 0 — a receive must never touch the send total", i+1, out)
		}
		if in != wantCumulative[i] {
			t.Fatalf(`after event %d: in = %d, want %d.

THE PER-EVENT VALUE FOR THIS EVENT IS %d. Reporting %d here means the
accumulator is forwarding ETW's per-event byte counts (%v) instead of the
CUMULATIVE running totals (%v) that processobs.MetricSample.NetRxBytes is
documented to carry and that every consumer differentiates. That failure is
silent in production — no error, no log, the chart just reads low by a factor
of the sampling interval. See docs/plans/process-obs-etw-windows-parity-plan-2026-07-26.md §0.1.`,
				i+1, in, wantCumulative[i], perEvent[i], perEvent[i], perEvent, wantCumulative)
		}
	}
}

// TestAddSeparatesDirections pins that send and receive accumulate into
// independent totals: a mixed stream must not cross-contaminate.
func TestAddSeparatesDirections(t *testing.T) {
	t.Parallel()

	const pid = 7
	a := NewAccumulator(0)
	a.Add(pid, DirectionReceive, 10)
	a.Add(pid, DirectionSend, 3)
	a.Add(pid, DirectionReceive, 5)
	a.Add(pid, DirectionSend, 7)

	in, out, ok := a.NetworkBytes(pid)
	if !ok {
		t.Fatal("NetworkBytes reported no counter")
	}
	if in != 15 || out != 10 {
		t.Fatalf("got in=%d out=%d, want in=15 out=10", in, out)
	}
}

// TestAddIgnoresUncountableEvents is table-driven over every input Add refuses
// to fold in. Each row must leave the accumulator completely empty — not a
// zeroed entry, no entry — so an uncountable event cannot even create a pid.
func TestAddIgnoresUncountableEvents(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		pid  int
		dir  Direction
		n    int64
	}{
		{"zero bytes", 100, DirectionReceive, 0},
		{"negative bytes", 100, DirectionReceive, -5},
		{"pid zero is the idle process", 0, DirectionReceive, 512},
		{"negative pid", -1, DirectionSend, 512},
		{"unknown direction", 100, DirectionUnknown, 512},
		{"direction out of range", 100, Direction(99), 512},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := NewAccumulator(0)
			a.Add(tc.pid, tc.dir, tc.n)
			if got := a.Len(); got != 0 {
				t.Fatalf("Len = %d, want 0: an uncountable event must not create a pid entry", got)
			}
			if _, _, ok := a.NetworkBytes(tc.pid); ok {
				t.Fatal("NetworkBytes reported a counter for a pid that never had a countable event")
			}
		})
	}
}

// TestNetworkBytesHasNoCounterForAnUnknownPid pins the ACCUMULATOR's ok, which
// is deliberately narrower than processobs.NetworkBytesFunc's: here false means
// "I hold nothing for that pid", not "unmeasured". Capture.NetworkBytes is what
// translates the former into the latter, and TestCaptureNetworkBytesContract
// (capture_other_test.go) pins that translation.
func TestNetworkBytesHasNoCounterForAnUnknownPid(t *testing.T) {
	t.Parallel()

	a := NewAccumulator(0)
	a.Add(1, DirectionReceive, 10)

	in, out, ok := a.NetworkBytes(999)
	if ok {
		t.Fatalf("NetworkBytes(999) = (%d, %d, true), want ok=false for a pid the accumulator has never seen", in, out)
	}
	if in != 0 || out != 0 {
		t.Fatalf("NetworkBytes(999) returned (%d, %d) alongside ok=false; the values must be zero", in, out)
	}
}

// TestForgetPreventsPidReuseInheritance is the pid-reuse guard. Windows recycles
// pids aggressively, so a new process landing on a recycled pid must start from
// zero — inheriting the previous occupant's totals would put a fabricated spike
// on its first chart point.
//
// Forget is the EXEC path and must clear BOTH stores: the live counters and the
// recently-exited cache. Clearing only the live half would leave the exit cache
// answering for the new occupant, which is the same bug wearing a hat.
func TestForgetPreventsPidReuseInheritance(t *testing.T) {
	t.Parallel()

	const pid = 5150

	t.Run("clears live counters", func(t *testing.T) {
		t.Parallel()
		a := NewAccumulator(0)
		a.Add(pid, DirectionReceive, 4096)
		a.Forget(pid)

		if in, out, ok := a.NetworkBytes(pid); ok {
			t.Fatalf("after Forget: NetworkBytes = (%d, %d, true), want no counter — the reused pid would inherit %d bytes", in, out, in)
		}
		if got := a.Len(); got != 0 {
			t.Fatalf("after Forget: Len = %d, want 0", got)
		}
	})

	t.Run("clears the recently-exited cache too", func(t *testing.T) {
		t.Parallel()
		a := NewAccumulator(0)
		a.Add(pid, DirectionSend, 4096)
		a.Retire(pid) // the previous occupant exits; totals move to the cache
		if _, _, ok := a.NetworkBytes(pid); !ok {
			t.Fatal("precondition: the retired cache should still answer for the exited pid")
		}

		a.Forget(pid) // a new process execs onto the recycled pid

		if in, out, ok := a.NetworkBytes(pid); ok {
			t.Fatalf("after Forget: the exit cache still answers (%d, %d) for a recycled pid", in, out)
		}
		if got := a.retiredLen(); got != 0 {
			t.Fatalf("retiredLen = %d, want 0", got)
		}
	})

	t.Run("the new occupant accumulates from zero", func(t *testing.T) {
		t.Parallel()
		a := NewAccumulator(0)
		a.Add(pid, DirectionReceive, 1_000_000)
		a.Retire(pid)
		a.Forget(pid)

		a.Add(pid, DirectionReceive, 42)
		in, _, ok := a.NetworkBytes(pid)
		if !ok {
			t.Fatal("NetworkBytes reported no counter for the new occupant")
		}
		if in != 42 {
			t.Fatalf("in = %d, want 42 — the new occupant inherited the previous process's bytes", in)
		}
	})
}

// TestRetireServesFinalTotals covers the exit path. The poll backend notices an
// exit up to one interval late and still samples the pid, so the final totals
// must outlive the live entry; otherwise the last chart point drops to zero and
// reads as a counter reset. Mirrors linuxebpf's netFinal cache.
func TestRetireServesFinalTotals(t *testing.T) {
	t.Parallel()

	const pid = 31337
	a := NewAccumulator(0)
	a.Add(pid, DirectionReceive, 700)
	a.Add(pid, DirectionSend, 300)
	a.Retire(pid)

	if got := a.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0: Retire must drop the LIVE entry", got)
	}
	in, out, ok := a.NetworkBytes(pid)
	if !ok {
		t.Fatal("after Retire: NetworkBytes reported no counter — the exit sample would read zero and look like a counter reset")
	}
	if in != 700 || out != 300 {
		t.Fatalf("after Retire: got in=%d out=%d, want in=700 out=300", in, out)
	}
}

// TestRetireOfAnUnknownPidRemembersNothing pins that retiring a pid with no
// live counters does NOT insert a zero. A zero in the exit cache would claim a
// measurement that never happened.
func TestRetireOfAnUnknownPidRemembersNothing(t *testing.T) {
	t.Parallel()

	a := NewAccumulator(0)
	a.Retire(12345)
	if got := a.retiredLen(); got != 0 {
		t.Fatalf("retiredLen = %d, want 0", got)
	}
	if _, _, ok := a.NetworkBytes(12345); ok {
		t.Fatal("NetworkBytes answers for a pid that never had a countable event")
	}
}

// TestLiveEntriesAreBounded pins the memory bound. A box with more chatty
// processes than the cap must EVICT rather than grow without bound or start
// refusing work.
//
// Eviction makes a cumulative counter DECREASE for the evicted pid, which is
// safe by an existing guard rather than by luck:
// internal/intelligence/dashboard/process.go's accumulateCounters drops a
// sample pair whose delta is negative (`if delta < 0 { continue }`), the same
// counter-reset handling pid reuse and a daemon restart already rely on. The
// cost of an eviction is therefore one interval's rate for that process.
func TestLiveEntriesAreBounded(t *testing.T) {
	t.Parallel()

	const max = 64
	a := NewAccumulator(max)
	for pid := 1; pid <= max*3; pid++ {
		a.Add(pid, DirectionReceive, int64(pid))
	}

	if got := a.Len(); got != max {
		t.Fatalf("Len = %d, want %d: the live map is not bounded", got, max)
	}
	// Least-recently-touched first: the oldest pids are gone, the newest kept.
	if _, _, ok := a.NetworkBytes(1); ok {
		t.Error("pid 1 survived; eviction is not least-recently-used")
	}
	newest := max * 3
	in, _, ok := a.NetworkBytes(newest)
	if !ok {
		t.Fatalf("the most recently active pid %d was evicted", newest)
	}
	if in != int64(newest) {
		t.Fatalf("pid %d: in = %d, want %d", newest, in, newest)
	}
}

// TestReadingAPidKeepsItFromBeingEvicted pins the touch-on-read half of the LRU
// policy: the pids being READ are exactly the ones being charted, so a
// quiet-but-watched process must not be evicted underneath its own chart by a
// burst of chatty strangers.
func TestReadingAPidKeepsItFromBeingEvicted(t *testing.T) {
	t.Parallel()

	const max = 8
	const watched = 1
	a := NewAccumulator(max)
	a.Add(watched, DirectionReceive, 11)

	for pid := 100; pid < 100+max*2; pid++ {
		a.Add(pid, DirectionReceive, 1)
		if _, _, ok := a.NetworkBytes(watched); !ok {
			t.Fatalf("the watched pid was evicted while being read (after %d chatty pids)", pid-99)
		}
	}

	in, _, ok := a.NetworkBytes(watched)
	if !ok || in != 11 {
		t.Fatalf("watched pid: got (%d, ok=%v), want (11, ok=true)", in, ok)
	}
	if got := a.Len(); got != max {
		t.Fatalf("Len = %d, want %d", got, max)
	}
}

// TestRetiredCacheIsBounded pins the exit cache's own bound, evicted
// oldest-first exactly like linuxebpf's netFinal/netOrder pair.
func TestRetiredCacheIsBounded(t *testing.T) {
	t.Parallel()

	a := NewAccumulator(0)
	total := retiredCacheMax + 50
	for pid := 1; pid <= total; pid++ {
		a.Add(pid, DirectionReceive, int64(pid))
		a.Retire(pid)
	}

	if got := a.retiredLen(); got != retiredCacheMax {
		t.Fatalf("retiredLen = %d, want %d", got, retiredCacheMax)
	}
	if len(a.retiredOrder) != retiredCacheMax {
		t.Fatalf("retiredOrder = %d entries, want %d — the order slice must stay in step with the map", len(a.retiredOrder), retiredCacheMax)
	}
	if _, _, ok := a.NetworkBytes(1); ok {
		t.Error("the oldest retired pid survived; the cache is not evicting oldest-first")
	}
	in, _, ok := a.NetworkBytes(total)
	if !ok || in != int64(total) {
		t.Fatalf("newest retired pid %d: got (%d, ok=%v)", total, in, ok)
	}
}

// TestForgetKeepsTheRetiredOrderInStep guards the one place the exit cache's
// two data structures can drift: Forget deletes from the map and must delete
// from the insertion-order slice too, or the slice grows unboundedly and
// evicts the wrong (already-absent) pids.
func TestForgetKeepsTheRetiredOrderInStep(t *testing.T) {
	t.Parallel()

	a := NewAccumulator(0)
	for pid := 1; pid <= 5; pid++ {
		a.Add(pid, DirectionReceive, 1)
		a.Retire(pid)
	}
	a.Forget(3)

	if got := a.retiredLen(); got != 4 {
		t.Fatalf("retiredLen = %d, want 4", got)
	}
	if len(a.retiredOrder) != 4 {
		t.Fatalf("retiredOrder = %v, want 4 entries with 3 removed", a.retiredOrder)
	}
	for _, pid := range a.retiredOrder {
		if pid == 3 {
			t.Fatalf("retiredOrder still lists the forgotten pid: %v", a.retiredOrder)
		}
	}
}

// TestNilAccumulatorIsInert pins that every method tolerates a nil receiver, so
// a caller holding an unpopulated field never has to nil-check.
func TestNilAccumulatorIsInert(t *testing.T) {
	t.Parallel()

	var a *Accumulator
	a.Add(1, DirectionReceive, 10)
	a.Forget(1)
	a.Retire(1)
	if got := a.Len(); got != 0 {
		t.Errorf("Len = %d, want 0", got)
	}
	if got := a.retiredLen(); got != 0 {
		t.Errorf("retiredLen = %d, want 0", got)
	}
	if in, out, ok := a.NetworkBytes(1); ok || in != 0 || out != 0 {
		t.Errorf("NetworkBytes = (%d, %d, %v), want (0, 0, false)", in, out, ok)
	}
}

// TestConcurrentAddsLoseNoBytes proves the mutex actually protects the
// read-modify-write of a pid's totals, not merely that -race is quiet.
//
// ETW delivers events on its own thread through the syscall.NewCallback
// trampoline while the metric sampler reads from another goroutine, so the
// accumulator is genuinely concurrent. An unsynchronised `entry.totals.in += n`
// loses updates under contention: this test asserts an exact total, so a lost
// update fails it deterministically even on a build without the race detector.
func TestConcurrentAddsLoseNoBytes(t *testing.T) {
	t.Parallel()

	const (
		pid        = 909
		goroutines = 16
		perG       = 500
	)

	a := NewAccumulator(0)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				a.Add(pid, DirectionReceive, 1)
			}
		}()
	}
	wg.Wait()

	in, _, ok := a.NetworkBytes(pid)
	if !ok {
		t.Fatal("NetworkBytes reported no counter")
	}
	if want := int64(goroutines * perG); in != want {
		t.Fatalf("in = %d, want %d: %d byte(s) were lost to an unsynchronised increment", in, want, want-in)
	}
}

// TestAccumulatorIsRaceFree exercises every method concurrently — the exact
// shape production has: the ETW callback thread calling Add, the metric
// sampler's goroutine calling NetworkBytes and Len, and the lifecycle path
// calling Forget and Retire.
//
// It asserts nothing about values on purpose; its assertion is the race
// detector. Verified to BITE: with the mutex removed from Accumulator's methods
// this test reports "WARNING: DATA RACE" under -race (see the W2 report).
func TestAccumulatorIsRaceFree(t *testing.T) {
	t.Parallel()

	const (
		workers = 8
		rounds  = 300
		pidMod  = 32
	)

	a := NewAccumulator(16) // small enough that eviction runs concurrently too
	var wg sync.WaitGroup
	wg.Add(workers * 4)

	for w := 0; w < workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				a.Add((w*rounds+i)%pidMod+1, DirectionReceive, int64(i%7)+1)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				a.Add((w*rounds+i)%pidMod+1, DirectionSend, 1)
				_ = a.Len()
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				_, _, _ = a.NetworkBytes((w*rounds + i) % pidMod)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				pid := (w*rounds+i)%pidMod + 1
				if i%2 == 0 {
					a.Retire(pid)
					continue
				}
				a.Forget(pid)
			}
		}()
	}
	wg.Wait()
}
