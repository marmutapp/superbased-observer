package proxy

import (
	"sync"
	"time"
)

// egressBreaker is a small per-target circuit breaker for Plane-A egress
// route_upstream targets (G22 wave 2, design §3.6 / §6). The proxy is the ONLY
// component that observes an egress target's runtime availability (design
// finding 1): the store-side passive health snapshot covers the router's
// same-shape model candidates, NOT arbitrary Plane-A upstream targets. This
// breaker gives the egress path its own availability signal so a repeatedly-dead
// target is short-circuited BEFORE the dial — a MustUseTarget locality route
// fails CLOSED immediately (never hangs), and a fail-open route falls back to
// the default upstream immediately (never hangs).
//
// It is deliberately minimal (no half-open probe storms, no metrics): count
// consecutive failures per target key; OPEN after failThreshold; after openFor
// elapses, allow a single trial (half-open) whose outcome re-opens or closes the
// breaker. Keyed by the target host so every rule pointing at the same endpoint
// shares one breaker.
type egressBreaker struct {
	mu            sync.Mutex
	states        map[string]*breakerState
	failThreshold int
	openFor       time.Duration
	now           func() time.Time
}

type breakerState struct {
	failures int
	openedAt time.Time
	open     bool
}

// newEgressBreaker returns a breaker with sensible defaults: OPEN after 3
// consecutive failures, stay open for 30s, then allow one trial.
func newEgressBreaker() *egressBreaker {
	return &egressBreaker{
		states:        map[string]*breakerState{},
		failThreshold: 3,
		openFor:       30 * time.Second,
		now:           time.Now,
	}
}

// Allow reports whether a request may dial the target key now. It returns false
// while the breaker is OPEN and the open window has not elapsed; once the window
// elapses it returns true for a single trial (half-open) and leaves the breaker
// marked open until the trial's outcome is recorded. An empty key always allows
// (nothing to protect).
func (b *egressBreaker) Allow(key string) bool {
	if b == nil || key == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.states[key]
	if st == nil || !st.open {
		return true
	}
	// Open: permit a single trial once the cool-off has elapsed.
	return b.now().Sub(st.openedAt) >= b.openFor
}

// RecordSuccess clears the breaker for key — a good response resets the failure
// count and closes the breaker.
func (b *egressBreaker) RecordSuccess(key string) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if st := b.states[key]; st != nil {
		st.failures = 0
		st.open = false
	}
}

// RecordFailure counts one failure for key and opens the breaker at the
// threshold (or immediately re-opens a half-open trial that failed).
func (b *egressBreaker) RecordFailure(key string) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.states[key]
	if st == nil {
		st = &breakerState{}
		b.states[key] = st
	}
	st.failures++
	if st.open || st.failures >= b.failThreshold {
		st.open = true
		st.openedAt = b.now()
	}
}

// Record folds one outcome into the breaker (success clears, failure counts).
func (b *egressBreaker) Record(key string, ok bool) {
	if ok {
		b.RecordSuccess(key)
	} else {
		b.RecordFailure(key)
	}
}
