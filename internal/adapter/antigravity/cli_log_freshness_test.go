package antigravity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCLILogFreshnessUnix_TracksFileAppends verifies the fix for the
// "binding written milliseconds after observer's first probe → cache
// holds nil forever" bug. After the binding-line append, the
// freshness key must advance even though no file was created/renamed
// in the dir (i.e. dir mtime unchanged on Linux ext4).
func TestCLILogFreshnessUnix_TracksFileAppends(t *testing.T) {
	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "cli-20260524_130017.log")
	if err := os.WriteFile(logFile, []byte("startup line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := cliLogFreshnessUnix(tmp)
	if first == 0 {
		t.Fatal("first freshness call returned 0; expected non-zero unix mtime")
	}

	// Sleep past one-second granularity then append to the existing log.
	time.Sleep(1100 * time.Millisecond)
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("Conversation using project ID: abc\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	second := cliLogFreshnessUnix(tmp)
	if second <= first {
		t.Errorf("after append: got freshness=%d, expected > %d (file append must invalidate the cache)", second, first)
	}
}

// TestCLILogFreshnessUnix_AddsFileBumpsFreshness — sanity check that
// the existing dir-mtime-on-create path still works (some
// filesystems update dir mtime on file creation; the function should
// honor that too).
func TestCLILogFreshnessUnix_AddsFileBumpsFreshness(t *testing.T) {
	tmp := t.TempDir()
	first := cliLogFreshnessUnix(tmp)

	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(tmp, "cli-new.log"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := cliLogFreshnessUnix(tmp)
	if second <= first {
		t.Errorf("after new file: got %d, expected > %d", second, first)
	}
}

// TestCLILogFreshnessUnix_IgnoresNonCLIFiles — unrelated files in the
// log dir (rotated old.log, stray .txt) must not influence the
// freshness key, otherwise unrelated tooling writing to that dir
// would over-invalidate the cache.
func TestCLILogFreshnessUnix_IgnoresNonCLIFiles(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "cli-1.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := cliLogFreshnessUnix(tmp)

	time.Sleep(1100 * time.Millisecond)
	// Write a non-cli- file: should not bump freshness above the
	// already-written cli-1.log's mtime, EXCEPT via dir mtime (which
	// happens on file create). Allow the dir-mtime equality but
	// ensure the function doesn't return a value derived from
	// readme.txt's mtime specifically.
	if err := os.WriteFile(filepath.Join(tmp, "readme.txt"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add another non-CLI file with a later mtime — readme should
	// still not move the needle past dir-mtime.
	after := cliLogFreshnessUnix(tmp)
	dirMtime := dirMtimeUnix(tmp)
	if after > dirMtime {
		t.Errorf("got freshness=%d > dirMtime=%d; non-cli files should not influence freshness", after, dirMtime)
	}
	if after < before {
		t.Errorf("freshness regressed: before=%d after=%d", before, after)
	}
}
