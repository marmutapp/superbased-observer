package main

import (
	"os"
	"runtime"
	"testing"
)

func TestIsDeletedTarget(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"/home/user/.observer/observer.db", false},
		{"/home/user/.observer/observer.db (deleted)", true},
		{"", false},
		{"(deleted)", false}, // no preceding space — not the kernel's shape
	}
	for _, c := range cases {
		if got := isDeletedTarget(c.target); got != c.want {
			t.Errorf("isDeletedTarget(%q) = %v, want %v", c.target, got, c.want)
		}
	}
}

func TestSumDeletedOpenFDBytesLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only behavior")
	}
	f, err := os.CreateTemp(t.TempDir(), "tempwatch-deleted-fd-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	const n = 4096
	if _, err := f.Write(make([]byte, n)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		t.Fatalf("remove (unlink while open): %v", err)
	}

	total, err := sumDeletedOpenFDBytes()
	if err != nil {
		t.Fatalf("sumDeletedOpenFDBytes: %v", err)
	}
	if total < n {
		t.Fatalf("sumDeletedOpenFDBytes = %d, want >= %d (our own deleted-but-open fd)", total, n)
	}
}

func TestSumDeletedOpenFDBytesNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux is exercised by TestSumDeletedOpenFDBytesLinux")
	}
	total, err := sumDeletedOpenFDBytes()
	if err != nil {
		t.Fatalf("sumDeletedOpenFDBytes: %v", err)
	}
	if total != 0 {
		t.Fatalf("sumDeletedOpenFDBytes on %s = %d, want 0", runtime.GOOS, total)
	}
}

func TestStaleBinaryWarningEmptyWhenCurrent(t *testing.T) {
	// The test binary itself is a live, non-deleted executable, so this
	// should report no warning under normal `go test` execution.
	if msg := staleBinaryWarning(); msg != "" {
		t.Fatalf("staleBinaryWarning() = %q, want \"\" for a live test binary", msg)
	}
}
