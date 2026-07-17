package stash

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStash_WriteRoundTrip(t *testing.T) {
	t.Parallel()
	s, err := New(Options{Dir: t.TempDir(), MaxTotalMB: 0})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := []byte("hello stash")
	sha, err := s.Write(body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sha != Sha(body) {
		t.Errorf("sha mismatch: %q vs %q", sha, Sha(body))
	}
	got, err := s.Read(sha)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-trip body mismatch: got %q want %q", got, body)
	}
}

func TestStash_WriteIsContentAddressed(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	body := []byte("identical content from two callers")

	first, err := s.Write(body)
	if err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	second, err := s.Write(body)
	if err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if first != second {
		t.Errorf("identical bodies produced different shas: %q vs %q", first, second)
	}

	entries, _ := os.ReadDir(s.Dir())
	count := 0
	for _, e := range entries {
		if !e.IsDir() && validSha(e.Name()) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("identical-body dedup failed: want 1 file on disk, got %d", count)
	}
}

func TestStash_ReadMissing(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	_, err := s.Read("0000000000000000000000000000000000000000000000000000000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing-blob: got %v, want ErrNotFound", err)
	}
}

func TestStash_ReadInvalidSha(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	for _, bad := range []string{"", "short", "../etc/passwd", "ABCDEF", "not-hex"} {
		if _, err := s.Read(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("invalid sha %q: got %v, want ErrNotFound", bad, err)
		}
	}
}

func TestStash_ReadCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := New(Options{Dir: dir})
	body := []byte("real content")
	sha, _ := s.Write(body)
	// Tamper with the file on disk.
	if err := os.WriteFile(filepath.Join(dir, sha), []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper write: %v", err)
	}
	_, err := s.Read(sha)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("corrupt-blob: got %v, want ErrCorrupt", err)
	}
}

func TestStash_GC_LRU(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 1MB cap; each blob is ~512KB so two fit, three don't.
	s, _ := New(Options{Dir: dir, MaxTotalMB: 1})
	half := make([]byte, 512*1024)
	for i := range half {
		half[i] = 'A'
	}
	first, _ := s.Write(half)
	// nudge mtimes so the ordering is reliable on filesystems with
	// 1-second mtime granularity (Linux ext4 default since 4.0).
	if err := os.Chtimes(filepath.Join(dir, first), time.Now().Add(-3*time.Second), time.Now().Add(-3*time.Second)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	half2 := make([]byte, 512*1024)
	for i := range half2 {
		half2[i] = 'B'
	}
	second, _ := s.Write(half2)
	if err := os.Chtimes(filepath.Join(dir, second), time.Now().Add(-2*time.Second), time.Now().Add(-2*time.Second)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Push us over cap.
	half3 := make([]byte, 512*1024)
	for i := range half3 {
		half3[i] = 'C'
	}
	if _, err := s.Write(half3); err != nil {
		t.Fatalf("Write 3: %v", err)
	}
	if err := s.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if _, err := s.Read(first); !errors.Is(err, ErrNotFound) {
		t.Errorf("oldest blob should have been evicted, got %v", err)
	}
	if _, err := s.Read(Sha(half3)); err != nil {
		t.Errorf("newest blob should have survived, got %v", err)
	}
}

func TestStash_GC_NoOpWhenUnderCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := New(Options{Dir: dir, MaxTotalMB: 100}) // 100MB cap, well over the few KB we'll write
	for _, b := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		if _, err := s.Write(b); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := s.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}
	for _, b := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		if _, err := s.Read(Sha(b)); err != nil {
			t.Errorf("blob %q evicted unexpectedly: %v", b, err)
		}
	}
}

func TestStash_NewRequiresDir(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}); err == nil {
		t.Error("New with empty Dir should error")
	}
}

func TestSha_Deterministic(t *testing.T) {
	t.Parallel()
	body := []byte("deterministic check")
	first := Sha(body)
	for i := 0; i < 100; i++ {
		if got := Sha(body); got != first {
			t.Fatalf("Sha non-deterministic on iteration %d: got %q want %q", i, got, first)
		}
	}
}

// TestReadSlice_FullBlobFastPath: startLine=0 && endLine=0 returns
// bytes byte-identical to Read, plus the total line count.
func TestReadSlice_FullBlobFastPath(t *testing.T) {
	t.Parallel()
	s, err := New(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := []byte("line1\nline2\nline3\n")
	sha, err := s.Write(body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	out, total, err := s.ReadSlice(sha, 0, 0)
	if err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("fast-path body: got %q, want %q", out, body)
	}
	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}
}

// TestReadSlice_FullRangeMatchesFastPath: ReadSlice(1, totalLines)
// produces byte-identical output to the fast path. Load-bearing for
// the V7-16 BC contract: explicit-full-range and implicit-full-range
// must be interchangeable.
func TestReadSlice_FullRangeMatchesFastPath(t *testing.T) {
	t.Parallel()
	for _, body := range [][]byte{
		[]byte("a\nb\nc\n"),          // trailing newline
		[]byte("alpha\nbeta\ngamma"), // no trailing newline
		[]byte("single\n"),
		[]byte("nonewline"),
		[]byte(""),
	} {
		s, _ := New(Options{Dir: t.TempDir()})
		sha, _ := s.Write(body)

		fast, total, err := s.ReadSlice(sha, 0, 0)
		if err != nil {
			t.Fatalf("fast-path %q: %v", body, err)
		}
		slow, _, err := s.ReadSlice(sha, 1, total)
		if err != nil {
			t.Fatalf("full-range %q: %v", body, err)
		}
		if !bytes.Equal(fast, slow) {
			t.Errorf("fast vs full-range mismatch for %q:\n  fast=%q\n  slow=%q", body, fast, slow)
		}
	}
}

// TestReadSlice_MidRange: an inclusive sub-range emits exactly those
// lines, each terminated by '\n', total reports the full blob's count.
func TestReadSlice_MidRange(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	body := []byte("L1\nL2\nL3\nL4\nL5\n")
	sha, _ := s.Write(body)

	out, total, err := s.ReadSlice(sha, 2, 4)
	if err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	if want := []byte("L2\nL3\nL4\n"); !bytes.Equal(out, want) {
		t.Errorf("got %q, want %q", out, want)
	}
	if total != 5 {
		t.Errorf("total: got %d, want 5", total)
	}
}

// TestReadSlice_NoTrailingNewline: blobs without a trailing '\n'
// (rare but legitimate — e.g. shell `echo -n` output) round-trip
// through the slice path correctly. Last-line slice preserves the
// no-trailing-newline status of the original.
func TestReadSlice_NoTrailingNewline(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	body := []byte("alpha\nbeta\ngamma") // 3 lines, no trailing \n
	sha, _ := s.Write(body)

	full, total, _ := s.ReadSlice(sha, 0, 0)
	if !bytes.Equal(full, body) || total != 3 {
		t.Errorf("full: got %q (%d lines), want %q (3)", full, total, body)
	}

	sliced, _, _ := s.ReadSlice(sha, 1, 3)
	if !bytes.Equal(sliced, body) {
		t.Errorf("full-range slice: got %q, want %q (trailing-\\n status preserved)", sliced, body)
	}

	mid, _, _ := s.ReadSlice(sha, 2, 2)
	if want := []byte("beta\n"); !bytes.Equal(mid, want) {
		t.Errorf("mid-line slice: got %q, want %q", mid, want)
	}

	last, _, _ := s.ReadSlice(sha, 3, 3)
	if want := []byte("gamma"); !bytes.Equal(last, want) {
		t.Errorf("last-line slice: got %q, want %q (no trailing \\n)", last, want)
	}
}

// TestReadSlice_StartPastEOF: a request for lines beyond the blob's
// extent returns empty bytes, no error, with total still populated so
// the agent can see the size mismatch.
func TestReadSlice_StartPastEOF(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	sha, _ := s.Write([]byte("a\nb\nc\n"))

	out, total, err := s.ReadSlice(sha, 10, 20)
	if err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %q, want empty (start past EOF)", out)
	}
	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}
}

// TestReadSlice_EndPastEOF: endLine beyond the blob's extent clamps to
// the actual line count.
func TestReadSlice_EndPastEOF(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	sha, _ := s.Write([]byte("a\nb\nc\n"))

	out, total, _ := s.ReadSlice(sha, 2, 99)
	if want := []byte("b\nc\n"); !bytes.Equal(out, want) {
		t.Errorf("got %q, want %q", out, want)
	}
	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}
}

// TestReadSlice_StartGreaterThanEnd: inverted range returns empty bytes
// (matches readSlice in internal/mcp/fileread.go for the equivalent
// case). No error — the agent can re-request with a corrected range.
func TestReadSlice_StartGreaterThanEnd(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	sha, _ := s.Write([]byte("a\nb\nc\nd\n"))

	out, total, err := s.ReadSlice(sha, 3, 2)
	if err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %q, want empty (start > end)", out)
	}
	if total != 4 {
		t.Errorf("total: got %d, want 4", total)
	}
}

// TestReadSlice_EndZeroMeansThroughEOF: endLine == 0 with a positive
// startLine reads from startLine through the end of the blob.
func TestReadSlice_EndZeroMeansThroughEOF(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	sha, _ := s.Write([]byte("a\nb\nc\nd\n"))

	out, total, _ := s.ReadSlice(sha, 3, 0)
	if want := []byte("c\nd\n"); !bytes.Equal(out, want) {
		t.Errorf("got %q, want %q", out, want)
	}
	if total != 4 {
		t.Errorf("total: got %d, want 4", total)
	}
}

// TestReadSlice_NotFound: missing sha surfaces ErrNotFound from Read,
// unmodified.
func TestReadSlice_NotFound(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	_, _, err := s.ReadSlice(strings.Repeat("0", 64), 1, 5)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestReadSlice_InvalidSha: malformed sha is rejected at the Read layer
// (the validSha guard against directory-traversal) — surfaces as
// ErrNotFound, never as a panic or path escape.
func TestReadSlice_InvalidSha(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	for _, bad := range []string{"", "short", "../etc/passwd", "ABCDEF", "not-hex"} {
		if _, _, err := s.ReadSlice(bad, 1, 5); !errors.Is(err, ErrNotFound) {
			t.Errorf("invalid sha %q: got %v, want ErrNotFound", bad, err)
		}
	}
}

// TestReadSlice_BumpsMtime: slice reads keep the underlying blob warm
// for LRU eviction, same as full Read. Without this, an agent doing
// repeated slice fetches on a stashed body would let the blob get
// GC'd between slices — exactly the Gap 2 failure mode.
func TestReadSlice_BumpsMtime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := New(Options{Dir: dir})
	sha, _ := s.Write([]byte("hello\nworld\n"))

	path := filepath.Join(dir, sha)
	past := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("backdate mtime: %v", err)
	}
	before, _ := os.Stat(path)

	if _, _, err := s.ReadSlice(sha, 1, 1); err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	after, _ := os.Stat(path)

	if !after.ModTime().After(before.ModTime()) {
		t.Errorf("mtime not bumped by ReadSlice: before=%v after=%v",
			before.ModTime(), after.ModTime())
	}
}

// TestReadSlice_EmptyBlob: zero-length body round-trips cleanly with
// total=0 and empty out across all range inputs.
func TestReadSlice_EmptyBlob(t *testing.T) {
	t.Parallel()
	s, _ := New(Options{Dir: t.TempDir()})
	sha, _ := s.Write([]byte{})

	for _, c := range []struct {
		name       string
		start, end int
		wantOut    []byte
	}{
		{"full-fast-path", 0, 0, []byte{}},
		{"any-range", 1, 5, []byte{}},
		{"start-zero", 0, 10, []byte{}},
	} {
		out, total, err := s.ReadSlice(sha, c.start, c.end)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(out) != 0 || total != 0 {
			t.Errorf("%s: got out=%q total=%d, want empty/0", c.name, out, total)
		}
	}
}

func TestStash_WriteCreatesDirOnFirstCall(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "stash") // intentionally missing
	s, _ := New(Options{Dir: dir})
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dir should not exist before first write: %v", err)
	}
	if _, err := s.Write([]byte("first")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir not created on first write: %v", err)
	}
}
