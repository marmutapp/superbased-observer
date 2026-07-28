package etw

import "sync/atomic"

// sessionHandles owns the two OS handles a trace session holds, plus the fact
// that a close has been requested.
//
// WHY THIS IS A TYPE AND WHY IT IS ATOMIC. A Session's documented usage is
// explicitly concurrent: Process blocks on its own goroutine and Close is what
// unblocks it, from another. So the consumer handle is written by Close while
// Process reads it, with nothing ordering the two. sync.Once gives Close
// idempotency, not ordering — a `go s.Process()` followed by a fast s.Close()
// could leave Process reading a zeroed handle and calling ProcessTrace with it,
// which fails ERROR_INVALID_HANDLE and turns a perfectly clean shutdown into a
// reported capture failure. It is also a plain -race violation.
//
// The handles live here, in an UNTAGGED file, rather than as fields on the
// Windows-only Session for one reason: CI has no Windows runner, so state that
// only exists behind //go:build windows can never be race-tested. Here it is
// exercised by handles_test.go under `go test -race` on every platform.
type sessionHandles struct {
	traceH   atomic.Uint64
	consumeH atomic.Uint64
	closing  atomic.Bool
}

// setTrace records StartTraceW's controller handle.
func (h *sessionHandles) setTrace(v uint64) { h.traceH.Store(v) }

// trace reports StartTraceW's controller handle, or 0 once it has been taken.
func (h *sessionHandles) trace() uint64 { return h.traceH.Load() }

// takeTrace atomically reads and clears the controller handle, so exactly one
// caller ever gets a non-zero value to stop.
func (h *sessionHandles) takeTrace() uint64 { return h.traceH.Swap(0) }

// setConsume records OpenTraceW's consumer handle.
func (h *sessionHandles) setConsume(v uint64) { h.consumeH.Store(v) }

// consume reports OpenTraceW's consumer handle, or 0 once a close has taken it.
func (h *sessionHandles) consume() uint64 { return h.consumeH.Load() }

// beginClose marks the session as shutting down and atomically takes the
// consumer handle, returning 0 if another caller already took it.
//
// Ordering matters and is the point of the method: closing is published BEFORE
// the handle is cleared, so a Process that observes a zeroed handle is
// guaranteed to also observe closing == true and can report the clean shutdown
// it actually is. The reverse order would leave a window where Process sees
// handle 0 and closing false, which is the bug this type exists to remove.
func (h *sessionHandles) beginClose() uint64 {
	h.closing.Store(true)
	return h.consumeH.Swap(0)
}

// closing reports whether a close has been requested. Named closingDown
// because closing is already the field.
func (h *sessionHandles) closingDown() bool { return h.closing.Load() }
