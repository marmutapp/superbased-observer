// Package dashboard — Phase-4 session-attach round-3 review regression tests:
// the F1b deadline-floor latency fix (a KNOWN positive deadline is honoured
// un-floored so expiry cancels a sensitive viewer promptly) and the strict
// terminal-settings decode rejecting a trailing JSON value.
package dashboard

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// firstLiveThenDeadLifetimer is a deviceSessionLifetimer that reports the
// session live (with a fixed `until`) on its FIRST poll and dead thereafter, so
// a test can observe exactly how long the watch loop armed its timer for on that
// first poll — the duration that distinguishes an un-floored positive deadline
// from a floored one.
type firstLiveThenDeadLifetimer struct {
	mu      sync.Mutex
	calls   int
	until   time.Duration
	revoked <-chan struct{}
}

func (f *firstLiveThenDeadLifetimer) SessionLifetime(string) (<-chan struct{}, time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 1 {
		return f.revoked, f.until, true
	}
	return f.revoked, 0, false
}

// TestWatchSessionLifetimePositiveDeadlineUnfloored pins the round-3 finding-1
// fix: a KNOWN positive deadline is NOT raised to the floor, so a bound sensitive
// viewer is cancelled at the deadline instead of up to a floor's-width later. A
// 20ms deadline under a 1s floor must cancel well before the floor would allow.
func TestWatchSessionLifetimePositiveDeadlineUnfloored(t *testing.T) {
	ch := make(chan struct{}) // never closed — TTL/idle expiry, not revoke
	lt := &firstLiveThenDeadLifetimer{until: 20 * time.Millisecond, revoked: ch}
	canceled := make(chan struct{})
	done := make(chan struct{})
	start := time.Now()
	// Large floor (1s): the OLD behaviour would raise the 20ms deadline to 1s.
	go watchSessionLifetime("raw", lt, done, func() { close(canceled) }, time.Second)

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("viewer not cancelled at the short positive deadline")
	}
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("cancel took %v — a known positive deadline (20ms) must NOT be raised to the 1s floor", elapsed)
	}
}

// TestWatchSessionLifetimeNonPositiveDeadlineFloored pins the retained spin
// guard: a non-positive (already-at/past-deadline) re-arm IS clamped to the
// floor, so the loop can't busy-spin re-reading liveness.
func TestWatchSessionLifetimeNonPositiveDeadlineFloored(t *testing.T) {
	ch := make(chan struct{})
	lt := &firstLiveThenDeadLifetimer{until: 0, revoked: ch}
	canceled := make(chan struct{})
	done := make(chan struct{})
	start := time.Now()
	go watchSessionLifetime("raw", lt, done, func() { close(canceled) }, 60*time.Millisecond)

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("viewer not cancelled after a non-positive deadline re-read")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("non-positive deadline re-read after %v — the floor must throttle it to ~60ms", elapsed)
	}
}

// TestTerminalSectionRejectsTrailingValueF4R3 pins the round-3 finding-2 fix: the
// terminal Settings section rejects a body with a trailing JSON value (the
// streaming decoder would otherwise decode the first object and silently drop
// the rest, reporting saved=true), while the still-valid single-object partial
// stays a 200.
func TestTerminalSectionRejectsTrailingValueF4R3(t *testing.T) {
	base := `[terminal.attach]
enabled = true
route_proxy = true
`
	// (1) A trailing (typo'd) object after the first ⇒ 400, not a silent no-op.
	rec := putTerminalSection(t, base, `{"Enabled":true}{"RouteProxi":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing-value body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// (2) A trailing SECOND VALID object is also rejected — one value only.
	if rec := putTerminalSection(t, base, `{"Enabled":true}{"RouteProxy":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing valid-object body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// (3) The single-object partial stays green (regression guard).
	if rec := putTerminalSection(t, base, `{"Enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("single-object partial = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
