package watcher

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestWarningDeduper_AllowsFirstAndSuppressesRepeatsWithinTTL pins the
// core V3-3 behavior: the same key emitted twice within the TTL
// produces exactly one Allow=true (the first), and the suppressed
// repeat returns false.
func TestWarningDeduper_AllowsFirstAndSuppressesRepeatsWithinTTL(t *testing.T) {
	d := newWarningDeduper(50 * time.Millisecond)
	if !d.Allow("antigravity|/p/foo.pb|OSCrypt failure") {
		t.Fatal("first call must be allowed")
	}
	if d.Allow("antigravity|/p/foo.pb|OSCrypt failure") {
		t.Fatal("identical key within TTL must be suppressed")
	}
	// A different message under the same path is a fresh signal.
	if !d.Allow("antigravity|/p/foo.pb|unrelated warning") {
		t.Fatal("different message must be allowed independently")
	}
	// A different path with the same message is a fresh signal.
	if !d.Allow("antigravity|/p/bar.pb|OSCrypt failure") {
		t.Fatal("different path must be allowed independently")
	}
}

// TestWarningDeduper_AllowsAgainAfterTTL ensures the warning still
// surfaces eventually — long-running daemons need periodic re-emission
// so a persistent failure doesn't go silent forever.
func TestWarningDeduper_AllowsAgainAfterTTL(t *testing.T) {
	d := newWarningDeduper(15 * time.Millisecond)
	if !d.Allow("k") {
		t.Fatal("first call must be allowed")
	}
	if d.Allow("k") {
		t.Fatal("repeat within TTL must be suppressed")
	}
	time.Sleep(25 * time.Millisecond)
	if !d.Allow("k") {
		t.Fatal("repeat after TTL must be re-allowed")
	}
}

// TestWarningDeduper_NilReceiverAlwaysAllows preserves the zero-config
// invariant — a Watcher constructed without explicit TTL config still
// works if dedup is wired later.
func TestWarningDeduper_NilReceiverAlwaysAllows(t *testing.T) {
	var d *warningDeduper
	if !d.Allow("anything") {
		t.Fatal("nil deduper must allow")
	}
}

// TestWarningDeduper_NonPositiveTTLDisables documents the escape
// hatch: when Options.AdapterWarningTTL is negative, every warning
// fires, which is the right behavior when an operator is diagnosing
// adapter chatter.
func TestWarningDeduper_NonPositiveTTLDisables(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		d := newWarningDeduper(ttl)
		// Same key, two consecutive calls — with dedup disabled both
		// must be allowed. Looping the assertion (rather than &&
		// linking) keeps each call distinct for staticcheck and makes
		// the intent ("every call allowed") explicit.
		for i := 0; i < 2; i++ {
			if !d.Allow("k") {
				t.Fatalf("ttl=%v: call #%d was suppressed; dedup should be disabled", ttl, i)
			}
		}
	}
}

// TestWarningDeduper_ConcurrentSafe protects against a future
// refactor accidentally dropping the mutex. The race detector catches
// the violation; the count assertion guards correctness.
func TestWarningDeduper_ConcurrentSafe(t *testing.T) {
	d := newWarningDeduper(time.Second)
	const (
		workers = 16
		hits    = 64
	)
	var wg sync.WaitGroup
	var allowed int64
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < hits; j++ {
				// Each worker uses its own keys so we can validate the
				// per-key behaviour even under concurrent access.
				key := fmt.Sprintf("worker-%d-key-%d", workerID, j%4)
				if d.Allow(key) {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()
	// 4 distinct keys per worker × workers workers = 4*workers allowed.
	if want := int64(4 * workers); allowed != want {
		t.Fatalf("allowed = %d, want %d distinct (worker,key) tuples", allowed, want)
	}
}
