package etw

import (
	"container/list"
	"sync"
)

// DefaultMaxEntries is the default cap on the number of pids the Accumulator
// tracks live counters for.
//
// It mirrors internal/processobs/linuxebpf's netMapMaxEntries (16384) on
// purpose: the Linux per-pid counter map is a BPF_MAP_TYPE_LRU_HASH with
// exactly that capacity, so a Windows box and a Linux box start forgetting the
// least-recently-active process at the same point rather than at two
// unrelated, undocumented ones.
const DefaultMaxEntries = 16384

// retiredCacheMax bounds the recently-exited counter cache. It mirrors
// linuxebpf's netFinalCacheMax (1024) for the same reason that constant exists:
// a process's final counters must outlive its live entry, because the poll
// backend only notices the exit up to one poll interval later. Without the
// cache the LAST point of every chart would drop to zero and be read as a
// counter reset.
const retiredCacheMax = 1024

// netTotals is one pid's cumulative (in, out) byte pair.
type netTotals struct{ in, out int64 }

// liveEntry is the value stored in the LRU list: a pid and its running totals.
// The list element is what the live map indexes, so a lookup is O(1) and a
// touch is a list splice rather than a scan.
type liveEntry struct {
	pid    int
	totals netTotals
}

// Accumulator turns ETW's PER-EVENT byte counts into the CUMULATIVE per-pid
// totals every processobs consumer expects. It is the single reason the
// Windows network numbers can be compared with the Linux ones at all.
//
// # Why this type exists (the arc's highest-risk bug)
//
// ETW reports the bytes moved by EACH event. The Linux eBPF backend reports a
// monotonic running total per pid — processobs.MetricSample.NetRxBytes /
// NetTxBytes are documented as cumulative since probe attach — and every
// consumer DIFFERENTIATES consecutive samples to derive a rate. Forward ETW's
// per-event numbers into that field and the charts are wrong by a factor of the
// sampling interval, with NO error, NO log line and NO failing test: the line
// simply reads low. Add is what stands between the two representations, and
// TestAddIsCumulativeNotPerEvent is the regression guard.
//
// # Concurrency
//
// ETW delivers events on its own thread, through the syscall.NewCallback
// trampoline, while the poll backend's metric sampler reads NetworkBytes from
// another goroutine and the process-lifecycle path calls Forget / Retire from a
// third. Every method therefore takes the same mutex. This is one lock, not a
// sharded or atomic scheme, because the critical sections are a map lookup and
// a list splice; contention is not the constraint, correctness is.
//
// # Bounded memory, and the decrease it can cause
//
// The live counters are an LRU capped at maxEntries: a box with more chatty
// processes than the cap EVICTS the least recently touched pid rather than
// growing without bound or refusing new work. Eviction — like pid reuse and a
// daemon restart — can make a "cumulative" counter DECREASE, because the next
// sample for an evicted pid starts again from zero.
//
// That is safe, and it is safe by an existing guard, not by luck:
// internal/intelligence/dashboard/process.go's accumulateCounters computes
// `delta := cv - pv` and then `if delta < 0 { continue }`, dropping that ONE
// sample pair for that ONE metric — the documented counter-reset handling. So
// an eviction costs a single interval's rate for that process, never a bogus
// negative rate and never a fabricated spike.
//
// # This type holds no processobs types
//
// The mode/reason vocabulary (processobs.NetworkAccounting*) belongs to
// Capture, which is the thing that knows whether accounting is live. The
// Accumulator is pure bookkeeping and imports nothing but the standard library,
// which is what keeps it testable on Linux where CI actually runs.
type Accumulator struct {
	mu         sync.Mutex
	maxEntries int

	// live indexes each tracked pid's element in lru. lru is ordered
	// most-recently-touched first, so eviction pops the back.
	live map[int]*list.Element
	lru  *list.List

	// retired holds the final totals of pids that have exited, bounded and
	// evicted in insertion order (oldest first) exactly like linuxebpf's
	// netFinal/netOrder pair.
	retired      map[int]netTotals
	retiredOrder []int
}

// NewAccumulator returns an Accumulator tracking at most maxEntries pids.
// A maxEntries of zero or less means DefaultMaxEntries.
func NewAccumulator(maxEntries int) *Accumulator {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Accumulator{
		maxEntries: maxEntries,
		live:       make(map[int]*list.Element),
		lru:        list.New(),
		retired:    make(map[int]netTotals, retiredCacheMax),
	}
}

// Add folds ONE event's byte count into a pid's running totals.
//
// n is the per-event value straight off TCPDataEvent.Bytes; the running total
// is what NetworkBytes reports. Feeding a stream of 100, 250, 90 for one pid
// therefore yields 100, then 350, then 440 — never 100, 250, 90.
//
// Ignored, deliberately and silently:
//
//   - n <= 0. A zero-byte event moves nothing; a negative one cannot come from
//     the decoder (the manifest's size field is a uint32 widened into an int64)
//     and would break monotonicity if it could.
//   - pid <= 0. Pid 0 is the Idle "process" and negative pids do not exist;
//     neither can own a socket. Pid 4 (System) IS a real process and is kept.
//   - DirectionUnknown, or any direction not in the table below. A byte count
//     whose direction is unknown cannot be added to either total without
//     inventing the answer.
//
// Overflow is not guarded because it is unreachable: the manifest's size field
// is a uint32, so one event contributes at most ~4 GiB and int64 would need
// more than 2^31 maximal events to wrap.
func (a *Accumulator) Add(pid int, dir Direction, n int64) {
	if a == nil || n <= 0 || pid <= 0 {
		return
	}
	if dir != DirectionReceive && dir != DirectionSend {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	el, ok := a.live[pid]
	if !ok {
		el = a.lru.PushFront(&liveEntry{pid: pid})
		a.live[pid] = el
	} else {
		a.lru.MoveToFront(el)
	}
	// The comma-ok form is not defensive noise: a bare assertion would panic
	// on a corrupted list, and CLAUDE.md forbids panic() in library code. The
	// nil branch is unreachable — this list only ever holds *liveEntry.
	entry, _ := el.Value.(*liveEntry)
	if entry == nil {
		return
	}
	switch dir {
	case DirectionReceive:
		entry.totals.in += n
	case DirectionSend:
		entry.totals.out += n
	case DirectionUnknown:
		// Unreachable: filtered above. Present so the switch is exhaustive
		// rather than relying on a default that would silently swallow a
		// future direction.
	}
	// Evict AFTER the update, and only ever from the back: the pid just
	// touched sits at the front, so a fresh event can never evict its own
	// entry.
	a.evictLocked()
}

// NetworkBytes reports the CUMULATIVE bytes this Accumulator has folded in for
// one pid, preferring the live counters and falling back to the recently-exited
// cache.
//
// READ THIS BEFORE WIRING IT ANYWHERE. ok here means "this Accumulator holds a
// counter for that pid" — it is NOT processobs.NetworkBytesFunc's ok, which
// means "accounting is live at all" and is TRUE with (0,0) for a measured but
// idle process. The two differ precisely for an unknown pid: this method says
// false ("I have nothing"), the processobs contract says (0,0,true)
// ("measured, and it moved nothing").
//
// Capture.NetworkBytes performs that translation and is the value that
// satisfies processobs.NetworkSampler. Passing an *Accumulator directly where a
// processobs.NetworkBytesFunc is wanted — it structurally fits, which is the
// hazard — would report every idle process as UNMEASURED and gap its chart. Use
// Capture.
func (a *Accumulator) NetworkBytes(pid int) (in, out int64, ok bool) {
	if a == nil {
		return 0, 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if el, hit := a.live[pid]; hit {
		// Touch on read as well as on write. The pids that get read are
		// exactly the ones being charted, so counting a read as "recently
		// used" is what stops a quiet-but-watched process from being evicted
		// underneath its own chart by a burst of chatty strangers. A read
		// never CREATES an entry — that would fill the map with zeroes for
		// every polled process and defeat the cap.
		a.lru.MoveToFront(el)
		if entry, _ := el.Value.(*liveEntry); entry != nil {
			return entry.totals.in, entry.totals.out, true
		}
	}
	if t, hit := a.retired[pid]; hit {
		return t.in, t.out, true
	}
	return 0, 0, false
}

// Forget drops EVERY counter this Accumulator holds for a pid — the live
// entry and the retired-cache entry both.
//
// It is the EXEC path, and dropping both is the whole point: Windows reuses
// pids, so without this a new process inheriting a recycled pid would inherit
// the previous occupant's byte totals and its first chart point would be a
// giant fabricated spike. linuxebpf does exactly this on sched_process_exec
// (delete from netFinal, then forget the map entry); Retire is the exit-path
// counterpart that keeps the totals instead.
func (a *Accumulator) Forget(pid int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropLiveLocked(pid)
	a.dropRetiredLocked(pid)
}

// Retire moves a pid's live counters into the bounded recently-exited cache.
//
// It is the EXIT path. The poll backend notices an exit up to one interval
// late, so it still samples the pid afterwards; serving the final totals from
// the cache keeps that last sample truthful instead of dropping it to zero,
// which the rate maths would read as a counter reset. Mirrors linuxebpf's
// rememberFinalLocked + forget pair.
//
// Retiring a pid with no live counters is a no-op: there is nothing to
// remember, and inserting a zero would claim a measurement that never happened.
func (a *Accumulator) Retire(pid int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	el, ok := a.live[pid]
	if !ok {
		return
	}
	if entry, _ := el.Value.(*liveEntry); entry != nil {
		a.rememberLocked(pid, entry.totals)
	}
	a.dropLiveLocked(pid)
}

// Len reports how many pids currently hold LIVE counters. Retired pids are not
// counted — they are a separate, separately bounded cache. It exists so the
// eviction bound is observable (and testable) rather than an unverified claim.
func (a *Accumulator) Len() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.live)
}

// retiredLen reports how many exited pids are held in the bounded final-totals
// cache. Unexported: it exists for the eviction tests, not for callers.
func (a *Accumulator) retiredLen() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.retired)
}

// evictLocked trims the live LRU back to maxEntries, dropping least-recently
// touched pids first. Caller holds mu.
func (a *Accumulator) evictLocked() {
	for len(a.live) > a.maxEntries {
		oldest := a.lru.Back()
		if oldest == nil {
			return
		}
		a.lru.Remove(oldest)
		if entry, _ := oldest.Value.(*liveEntry); entry != nil {
			delete(a.live, entry.pid)
		}
	}
}

// dropLiveLocked removes a pid's live entry if present. Caller holds mu.
func (a *Accumulator) dropLiveLocked(pid int) {
	el, ok := a.live[pid]
	if !ok {
		return
	}
	a.lru.Remove(el)
	delete(a.live, pid)
}

// dropRetiredLocked removes a pid from the recently-exited cache, keeping the
// insertion-order slice in step. Caller holds mu.
func (a *Accumulator) dropRetiredLocked(pid int) {
	if _, ok := a.retired[pid]; !ok {
		return
	}
	delete(a.retired, pid)
	for i, p := range a.retiredOrder {
		if p == pid {
			a.retiredOrder = append(a.retiredOrder[:i], a.retiredOrder[i+1:]...)
			break
		}
	}
}

// rememberLocked inserts into the bounded recently-exited cache, evicting
// oldest-first. Caller holds mu.
func (a *Accumulator) rememberLocked(pid int, t netTotals) {
	if _, dup := a.retired[pid]; !dup {
		a.retiredOrder = append(a.retiredOrder, pid)
	}
	a.retired[pid] = t
	for len(a.retiredOrder) > retiredCacheMax {
		delete(a.retired, a.retiredOrder[0])
		a.retiredOrder = a.retiredOrder[1:]
	}
}
