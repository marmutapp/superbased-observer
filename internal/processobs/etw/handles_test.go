package etw

import (
	"sync"
	"testing"
)

// TestHandlesConcurrentProcessAndClose is the race coverage for the one piece
// of genuinely concurrent state in this package.
//
// The bug it exists for: Session.Process reads the consumer handle while
// Session.Close clears it, from a different goroutine, and the package's
// documented usage is exactly that (Process blocks on its own goroutine, Close
// is what unblocks it). sync.Once gave Close idempotency but no ordering, so
// the two were unsynchronised by design.
//
// MUTATION PROOF (the only kind worth having): change sessionHandles' fields
// from atomic.Uint64 / atomic.Bool to plain uint64 / bool and this test fails
// under `go test -race` with WARNING: DATA RACE on sessionHandles.consumeH,
// naming the read in consume and the write in beginClose. Restore the atomics
// and it passes. A test that stays green either way is worthless.
//
// It runs on every platform on purpose: the state lives in an untagged file
// precisely so CI, which has no Windows runner, can race-test it.
func TestHandlesConcurrentProcessAndClose(t *testing.T) {
	t.Parallel()

	const rounds = 200
	for i := 0; i < rounds; i++ {
		var h sessionHandles
		h.setTrace(0xDEAD0000 + uint64(i))
		h.setConsume(0xBEEF0000 + uint64(i))

		var wg sync.WaitGroup
		wg.Add(2)

		// The Process side: read the handle, and if it is gone confirm the
		// close that took it is observable.
		go func() {
			defer wg.Done()
			if h.consume() == 0 && !h.closingDown() {
				t.Error("observed a cleared consumer handle without an observable close: " +
					"Process would report ERROR_INVALID_HANDLE for a clean shutdown")
			}
		}()

		// The Close side.
		go func() {
			defer wg.Done()
			h.beginClose()
			h.takeTrace()
		}()

		wg.Wait()
	}
}

// TestHandlesCloseOrderingIsObservable pins the ordering beginClose exists to
// guarantee: closing is published BEFORE the handle is cleared, so a reader
// that sees handle 0 is guaranteed to also see closingDown() == true. The
// reverse order leaves a window in which Process sees "no handle, not closing"
// and reports a bogus capture failure.
func TestHandlesCloseOrderingIsObservable(t *testing.T) {
	t.Parallel()

	var h sessionHandles
	h.setConsume(42)

	if h.closingDown() {
		t.Fatal("a fresh sessionHandles reports closing")
	}
	if got := h.beginClose(); got != 42 {
		t.Fatalf("beginClose() = %d, want the consumer handle 42", got)
	}
	if !h.closingDown() {
		t.Fatal("closingDown() = false after beginClose")
	}
	if got := h.consume(); got != 0 {
		t.Fatalf("consume() = %d after beginClose, want 0", got)
	}
}

// TestHandlesAreTakenExactlyOnce pins the CloseTrace/ControlTraceW invariant
// independently of Session.closeOnce: whichever caller wins, only one gets a
// non-zero handle to close, so the OS handle can never be closed twice.
func TestHandlesAreTakenExactlyOnce(t *testing.T) {
	t.Parallel()

	t.Run("sequential", func(t *testing.T) {
		t.Parallel()
		var h sessionHandles
		h.setConsume(7)
		h.setTrace(9)

		if got := h.beginClose(); got != 7 {
			t.Fatalf("first beginClose() = %d, want 7", got)
		}
		if got := h.beginClose(); got != 0 {
			t.Fatalf("second beginClose() = %d, want 0", got)
		}
		if got := h.takeTrace(); got != 9 {
			t.Fatalf("first takeTrace() = %d, want 9", got)
		}
		if got := h.takeTrace(); got != 0 {
			t.Fatalf("second takeTrace() = %d, want 0", got)
		}
		if h.trace() != 0 {
			t.Fatalf("trace() = %d after takeTrace, want 0", h.trace())
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()
		const closers = 16
		var h sessionHandles
		h.setConsume(1234)

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			nonZero int
		)
		wg.Add(closers)
		for i := 0; i < closers; i++ {
			go func() {
				defer wg.Done()
				if h.beginClose() != 0 {
					mu.Lock()
					nonZero++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if nonZero != 1 {
			t.Fatalf("%d of %d concurrent beginClose calls got the handle, want exactly 1", nonZero, closers)
		}
	})
}
