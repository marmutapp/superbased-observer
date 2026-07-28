package termsession

import "sync"

// defaultRingBytes bounds a session's replay ring (256 KiB). It holds the
// most-recent PTY output so a reconnecting client can repaint the screen +
// recent scrollback; older bytes are dropped once the ring is full. This is
// scrollback, not a transcript — the DB never stores terminal output.
const defaultRingBytes = 256 * 1024

// outBuf is a session's always-drained output buffer: a bounded byte ring
// plus a broadcast wake channel. The Manager's per-session pump goroutine is
// the sole writer (it drains the PTY whether or not a client is attached, so
// a clientless PTY never blocks); the attached client's Read is the sole
// reader. Offsets are ABSOLUTE over the whole session output stream — buf
// holds bytes [base, total); base advances as the ring trims. A reader tracks
// its own absolute cursor, so replay-then-tail is just "read from 0".
//
// Waking uses the closed-and-replaced channel idiom (not sync.Cond) so a
// reader can wait with `select { case <-ctx.Done(): ...; case <-wait: }` and
// compose cleanly with cancellation.
type outBuf struct {
	mu     sync.Mutex
	buf    []byte        // ring contents: absolute bytes [base, total)
	base   int64         // absolute offset of buf[0]
	total  int64         // absolute offset one past the last buffered byte
	max    int           // ring capacity in bytes
	notify chan struct{} // closed+replaced under mu on every write/close
	closed bool          // PTY drained (process exited or killed)
}

// newOutBuf builds an empty ring of the given capacity (<=0 ⇒ default).
func newOutBuf(max int) *outBuf {
	if max <= 0 {
		max = defaultRingBytes
	}
	return &outBuf{max: max, notify: make(chan struct{})}
}

// wakeLocked closes the current notify channel (releasing all waiters) and
// installs a fresh one. The caller MUST hold o.mu.
func (o *outBuf) wakeLocked() {
	close(o.notify)
	o.notify = make(chan struct{})
}

// write appends p to the ring, trims the oldest bytes past the capacity, and
// wakes any waiting reader. Called only by the pump.
func (o *outBuf) write(p []byte) {
	if len(p) == 0 {
		return
	}
	o.mu.Lock()
	o.buf = append(o.buf, p...)
	o.total += int64(len(p))
	if len(o.buf) > o.max {
		drop := len(o.buf) - o.max
		// Realloc-tight: copy the retained tail into a fresh slice so the
		// backing array can't grow without bound across trims.
		o.buf = append([]byte(nil), o.buf[drop:]...)
		o.base += int64(drop)
	}
	o.wakeLocked()
	o.mu.Unlock()
}

// close marks the stream drained (the PTY has ended) and wakes readers so a
// caught-up Read returns EOF instead of blocking. Idempotent.
func (o *outBuf) close() {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		o.wakeLocked()
	}
	o.mu.Unlock()
}

// read copies up to len(p) bytes at/after *off into p and advances *off by the
// number copied. When the reader is caught up it returns n==0 with the current
// wake channel to block on (nil once the stream is closed and fully drained).
// If *off has fallen behind the trimmed ring it is clamped forward to base and
// the number of DROPPED bytes is reported in `lost` — the drop-oldest fan-out
// discipline (§4.α.1): a slow/malicious subscriber is degraded with a visible
// gap (the subscriber's Lost counter), never a stall of the always-draining
// pump. The clamp keeps the invariant *off ∈ [base, total].
func (o *outBuf) read(off *int64, p []byte) (n int, wait <-chan struct{}, closed bool, lost int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if *off < o.base {
		lost = o.base - *off
		*off = o.base
	}
	if *off < o.total {
		n = copy(p, o.buf[*off-o.base:])
		*off += int64(n)
		return n, nil, false, lost
	}
	if o.closed {
		return 0, nil, true, lost
	}
	return 0, o.notify, false, lost
}

// currentBase returns the absolute offset of the oldest buffered byte under the
// ring lock. It is the FLOOR of a new subscriber's replay start (see
// Session.replayStart, which may start later — at the last geometry boundary),
// so replay-then-tail is one monotonic cursor over the SINGLE authoritative ring
// (§4.α.1) — there is no second buffer and thus no replay/live handoff seam to
// get wrong.
func (o *outBuf) currentBase() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.base
}

// currentTotal returns the absolute offset one past the last buffered byte
// under the ring lock, i.e. how many bytes the pump has drained into the ring
// so far. Session.recordResize captures it as the geometry boundary (everything
// before it was laid out at the OLD width); tests use it to wait for the pump to
// have fully drained a known amount of produced output before asserting on ring
// state.
func (o *outBuf) currentTotal() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total
}
