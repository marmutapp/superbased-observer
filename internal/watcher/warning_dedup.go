package watcher

import (
	"sync"
	"time"
)

// defaultAdapterWarningTTL is the dedup window for adapter warnings
// logged from processFile. The 5-minute default suppresses the v3
// antigravity log-spam pattern (the same OSCrypt / unrecoverable
// warning emitted every ~30 s of poll for as long as the file sits
// around — ~96% of stderr lines in V3 batch logs) while still
// surfacing the warning periodically so it's not silently lost.
// See docs/observer-platform-issues-v3.md V3-3.
const defaultAdapterWarningTTL = 5 * time.Minute

// warningDedupSweepThreshold is the entry count past which Allow runs
// a one-pass sweep of expired entries before deciding. Keeps the map
// from growing without bound when a long-running daemon sees a high
// churn of distinct (adapter,path,msg) tuples.
const warningDedupSweepThreshold = 1024

// warningDeduper tracks the last emission time of an (adapter, path,
// message) tuple so the watcher can suppress identical adapter warnings
// that repeat every poll cycle. The intended consumer is the loop
// inside processFile that logs each entry from
// adapter.ParseResult.Warnings.
type warningDeduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

// newWarningDeduper builds a deduper with the given TTL. A non-positive
// TTL produces a deduper whose Allow always returns true — caller-side
// suppression is disabled. Useful for tests that want to assert every
// warning fires.
func newWarningDeduper(ttl time.Duration) *warningDeduper {
	return &warningDeduper{
		seen: make(map[string]time.Time),
		ttl:  ttl,
	}
}

// Allow reports whether the warning identified by key should be
// emitted. It returns true (and refreshes the timestamp) when this is
// the first observation within the TTL window; false otherwise. A nil
// receiver or non-positive TTL is treated as "always allow" so callers
// don't need a nil-check.
func (d *warningDeduper) Allow(key string) bool {
	if d == nil || d.ttl <= 0 {
		return true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.seen) >= warningDedupSweepThreshold {
		for k, t := range d.seen {
			if now.Sub(t) >= d.ttl {
				delete(d.seen, k)
			}
		}
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) < d.ttl {
		return false
	}
	d.seen[key] = now
	return true
}
