package dblease

import "testing"

// TestTryAcquire_Exclusive proves the core contract: a second TryAcquire
// for the same name fails (acquired=false, err=nil) while the first is
// held, and a later TryAcquire succeeds once the first is released. Only
// meaningful where Available() (real flock semantics); off unix the
// degrade-to-always-true behavior is exercised instead, so exclusivity is
// deliberately not asserted there — see [TryAcquire]'s doc comment.
func TestTryAcquire_Exclusive(t *testing.T) {
	if !Available() {
		t.Skip("no real cross-process lease on this platform (degrades to always-acquired)")
	}
	dir := t.TempDir()

	release1, acquired1, err := TryAcquire(dir, "test-lease")
	if err != nil {
		t.Fatalf("first TryAcquire: unexpected error: %v", err)
	}
	if !acquired1 {
		t.Fatalf("first TryAcquire: want acquired=true, got false")
	}

	_, acquired2, err := TryAcquire(dir, "test-lease")
	if err != nil {
		t.Fatalf("second TryAcquire: unexpected error: %v", err)
	}
	if acquired2 {
		t.Fatalf("second TryAcquire: want acquired=false while the first holder still has it, got true")
	}

	release1()

	release3, acquired3, err := TryAcquire(dir, "test-lease")
	if err != nil {
		t.Fatalf("TryAcquire after release: unexpected error: %v", err)
	}
	if !acquired3 {
		t.Fatalf("TryAcquire after release: want acquired=true, got false")
	}
	release3()
}

// TestTryAcquire_DifferentNamesIndependent proves two different lease
// names under the same dir don't contend with each other.
func TestTryAcquire_DifferentNamesIndependent(t *testing.T) {
	dir := t.TempDir()

	releaseA, acquiredA, err := TryAcquire(dir, "lease-a")
	if err != nil || !acquiredA {
		t.Fatalf("acquire lease-a: acquired=%v err=%v", acquiredA, err)
	}
	defer releaseA()

	releaseB, acquiredB, err := TryAcquire(dir, "lease-b")
	if err != nil || !acquiredB {
		t.Fatalf("acquire lease-b: acquired=%v err=%v", acquiredB, err)
	}
	defer releaseB()
}

// TestTryAcquire_EmptyName rejects an empty lease name rather than
// silently building a lock file called ".lock".
func TestTryAcquire_EmptyName(t *testing.T) {
	dir := t.TempDir()
	_, acquired, err := TryAcquire(dir, "")
	if err == nil {
		t.Fatalf("want error for empty name, got nil")
	}
	if acquired {
		t.Fatalf("want acquired=false for empty name, got true")
	}
}
