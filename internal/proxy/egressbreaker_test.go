package proxy

import (
	"testing"
	"time"
)

func TestEgressBreakerOpensAndRecovers(t *testing.T) {
	now := time.Unix(0, 0)
	b := newEgressBreaker()
	b.now = func() time.Time { return now }
	b.failThreshold = 3
	b.openFor = 30 * time.Second

	const key = "127.0.0.1:11434"

	// Below threshold: still allowed.
	b.RecordFailure(key)
	b.RecordFailure(key)
	if !b.Allow(key) {
		t.Fatal("breaker opened before the failure threshold")
	}
	// At threshold: opens → no longer allowed within the window.
	b.RecordFailure(key)
	if b.Allow(key) {
		t.Fatal("breaker did not open at the failure threshold")
	}
	// Still open before the cool-off elapses.
	now = now.Add(29 * time.Second)
	if b.Allow(key) {
		t.Fatal("breaker allowed a dial before the cool-off elapsed")
	}
	// After cool-off: a single trial is permitted (half-open).
	now = now.Add(2 * time.Second)
	if !b.Allow(key) {
		t.Fatal("breaker did not permit a trial after the cool-off")
	}
	// The trial succeeds → breaker closes and stays closed.
	b.RecordSuccess(key)
	if !b.Allow(key) {
		t.Fatal("breaker did not close after a successful trial")
	}
}

func TestEgressBreakerHalfOpenTrialFailureReopens(t *testing.T) {
	now := time.Unix(0, 0)
	b := newEgressBreaker()
	b.now = func() time.Time { return now }
	b.failThreshold = 1
	b.openFor = 10 * time.Second

	const key = "host:1"
	b.RecordFailure(key) // opens immediately (threshold 1)
	if b.Allow(key) {
		t.Fatal("breaker should be open")
	}
	now = now.Add(11 * time.Second)
	if !b.Allow(key) {
		t.Fatal("half-open trial not permitted after cool-off")
	}
	// Trial fails → re-opens, and the window resets.
	b.RecordFailure(key)
	if b.Allow(key) {
		t.Fatal("failed half-open trial did not re-open the breaker")
	}
}

func TestEgressBreakerNilAndEmptyKeyAllow(t *testing.T) {
	var b *egressBreaker // nil receiver is safe (zero-overhead default)
	if !b.Allow("x") {
		t.Fatal("nil breaker must allow")
	}
	b.RecordFailure("x") // must not panic
	b.RecordSuccess("x")

	real := newEgressBreaker()
	if !real.Allow("") {
		t.Fatal("empty key must always allow (nothing to protect)")
	}
}
