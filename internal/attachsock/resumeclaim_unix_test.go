//go:build unix

package attachsock

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestResumeClaimConflictAndRelease pins the H3 durable-claim contract: the
// first acquire succeeds, a second acquire of the SAME session conflicts
// (ok=false, no error) while held, and after Release the session is claimable
// again. A DIFFERENT session never contends.
func TestResumeClaimConflictAndRelease(t *testing.T) {
	dir := t.TempDir()

	c1, ok, err := AcquireResumeClaim(dir, "sess-A")
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v, want ok=true", ok, err)
	}

	// A second acquire of the same session — from a distinct open file
	// description — must observe the held flock and report a conflict, not error.
	c2, ok, err := AcquireResumeClaim(dir, "sess-A")
	if err != nil {
		t.Fatalf("conflicting acquire errored: %v (want a clean ok=false)", err)
	}
	if ok {
		t.Fatal("a second acquire of a held session must report a conflict (ok=false)")
	}
	if c2 != nil {
		t.Fatal("a conflicting acquire must return a nil claim")
	}

	// A different session id never contends.
	cB, ok, err := AcquireResumeClaim(dir, "sess-B")
	if err != nil || !ok {
		t.Fatalf("different session acquire: ok=%v err=%v, want ok=true", ok, err)
	}
	cB.Release()

	// Releasing the first claim frees sess-A for a fresh acquire.
	c1.Release()
	c1.Release() // idempotent — must not panic or error
	c3, ok, err := AcquireResumeClaim(dir, "sess-A")
	if err != nil || !ok {
		t.Fatalf("re-acquire after release: ok=%v err=%v, want ok=true", ok, err)
	}
	c3.Release()
}

// TestResumeClaimStaleAfterCrashAutoReleases pins the flock property the H3 fix
// relies on for crash resilience: a claim held by a process that dies WITHOUT
// releasing is auto-cleared by the OS when its fd closes, so a subsequent
// acquire succeeds. We simulate the crash by taking the raw flock on a separate
// fd and closing it without an explicit LOCK_UN.
func TestResumeClaimStaleAfterCrashAutoReleases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resume-"+sanitizeSessionID("sess-crash")+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("raw flock: %v", err)
	}
	// While the raw lock is held, a claim must conflict.
	if _, ok, _ := AcquireResumeClaim(dir, "sess-crash"); ok {
		t.Fatal("expected a conflict while the raw flock is held")
	}
	// Simulate a crash: close the fd WITHOUT LOCK_UN. The OS releases the flock.
	_ = f.Close()

	c, ok, err := AcquireResumeClaim(dir, "sess-crash")
	if err != nil || !ok {
		t.Fatalf("acquire after simulated crash: ok=%v err=%v, want ok=true (OS auto-release)", ok, err)
	}
	c.Release()
}

// TestSanitizeSessionID keeps the lock filename filesystem-safe (no path
// separators smuggled into the attach dir).
func TestSanitizeSessionID(t *testing.T) {
	cases := map[string]string{
		"abc-123_x.y":  "abc-123_x.y",
		"a/b":          "a_b",
		"../evil":      ".._evil",
		"has space":    "has_space",
		"":             "_",
		"utf8-ünïcöde": "utf8-_n_c_de",
	}
	for in, want := range cases {
		if got := sanitizeSessionID(in); got != want {
			t.Errorf("sanitizeSessionID(%q) = %q, want %q", in, got, want)
		}
	}
}
