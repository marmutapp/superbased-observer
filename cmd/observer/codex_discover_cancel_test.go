package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// errAfterNCtx is a context whose Err() returns nil for the first n calls and
// context.Canceled thereafter, and whose Done() never fires. It lets a test
// drive runCodexDiscovery deterministically to the POST-LOOP ctx.Err() recheck
// (F1): with an already-elapsed window the loop breaks after its single scan
// (Done() is nil, so the select is never the exit path), and the recheck — the
// (n+1)th Err() call — is the one that observes the cancel.
type errAfterNCtx struct {
	context.Context
	mu    sync.Mutex
	calls int
	n     int
}

func (c *errAfterNCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

func (c *errAfterNCtx) Done() <-chan struct{} { return nil }

// TestRunCodexDiscoveryCancelWins covers F1: a discovery whose ctx is cancelled
// around window close must NOT announce, even with a single otherwise-valid
// candidate — cancel wins over a completed final scan.
func TestRunCodexDiscoveryCancelWins(t *testing.T) {
	cwd := "/work/proj"

	// cancel-during-final-scan ⇒ no announce. A single valid candidate is present
	// from the start; window=1ns means the deadline is already past, so the loop
	// breaks after exactly one scan. Err() is nil in-loop (call 1, n=1) and
	// Canceled at the post-loop recheck (call 2), so the recheck must abstain
	// even though the scan found a unique candidate. Without the recheck this
	// announces "solo-1".
	t.Run("cancel between final scan and announce ⇒ no announce", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "solo-1", cwd)
		ctx := &errAfterNCtx{Context: context.Background(), n: 1}
		cfg := codexDiscoverConfig{window: time.Nanosecond, poll: time.Hour}

		var calls int
		runCodexDiscovery(ctx, []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(string) { calls++ })
		if calls != 0 {
			t.Fatalf("cancel observed at the post-loop recheck must abstain, got %d announce(s)", calls)
		}
	})

	// child-exits-then-window-closes ⇒ no announce. The child exits mid-window
	// (real cancel, mimicking discCancel firing on child.Wait), so discovery
	// returns before its deadline — a valid single candidate is present but must
	// not be announced.
	t.Run("child exit mid-window ⇒ no announce", func(t *testing.T) {
		root := t.TempDir()
		start := time.Now().Add(-time.Second)
		writeRollout(t, root, "solo-2", cwd)
		ctx, cancel := context.WithCancel(context.Background())
		cfg := codexDiscoverConfig{window: 500 * time.Millisecond, poll: 5 * time.Millisecond}
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		var calls int
		runCodexDiscovery(ctx, []string{root}, map[string]struct{}{}, start, cwd, cfg,
			func(string) { calls++ })
		if calls != 0 {
			t.Fatalf("a child exit mid-window must abstain, got %d announce(s)", calls)
		}
	})
}
