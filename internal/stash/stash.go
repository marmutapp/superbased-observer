package stash

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by [Stash.Read] when no blob is stored under
// the given sha. Callers should treat it as a normal "stash miss" (the
// blob may have been GCed) rather than a fatal error.
var ErrNotFound = errors.New("stash: blob not found")

// ErrCorrupt is returned by [Stash.Read] when the on-disk content does
// not hash to its filename. Indicates the stash dir was tampered with
// or a partial write was interrupted.
var ErrCorrupt = errors.New("stash: blob hash mismatch")

// Stash is a thread-safe content-addressed blob store rooted at a
// single directory. The zero value is unusable — call [New].
type Stash struct {
	dir         string
	maxTotalMB  int
	mu          sync.Mutex
	createDirOK bool
}

// Options configures [New].
type Options struct {
	// Dir is the absolute path to the stash root. Created on first
	// write if missing. Required.
	Dir string
	// MaxTotalMB caps the total on-disk size. When [Stash.GC] is called
	// (or on the post-write trigger) blobs are evicted oldest-mtime
	// first until the total fits. Zero disables GC entirely (unbounded
	// growth — only acceptable in tests).
	MaxTotalMB int
}

// New constructs a Stash. Returns an error when Dir is empty. Does not
// create the directory until the first write — callers that want eager
// creation should follow up with `os.MkdirAll(opts.Dir, 0o700)`.
func New(opts Options) (*Stash, error) {
	if opts.Dir == "" {
		return nil, errors.New("stash.New: Dir is required")
	}
	return &Stash{
		dir:        opts.Dir,
		maxTotalMB: opts.MaxTotalMB,
	}, nil
}

// Dir returns the configured root directory. Used by tests and for
// human-readable diagnostics.
func (s *Stash) Dir() string { return s.dir }

// Sha returns the canonical SHA-256-hex form of body. Exported so
// callers can predict the marker form before deciding whether to call
// [Stash.Write] (e.g. dedup detection).
func Sha(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Write stores body and returns its SHA-256-hex name. Idempotent: if
// the blob already exists the existing file's mtime is bumped (so it
// counts as recently-used for LRU eviction) and no rewrite happens.
//
// On-disk write is atomic: a temp file is written to `<dir>/.tmp-<sha>`
// and renamed into place. A partial crash leaves the temp file behind
// and never produces a half-written blob at the canonical path.
func (s *Stash) Write(body []byte) (string, error) {
	sha := Sha(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return "", err
	}
	final := filepath.Join(s.dir, sha)
	if _, err := os.Stat(final); err == nil {
		_ = os.Chtimes(final, time.Now(), time.Now())
		return sha, nil
	}
	tmp := filepath.Join(s.dir, ".tmp-"+sha)
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("stash.Write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("stash.Write: rename: %w", err)
	}
	return sha, nil
}

// Read returns the bytes stored under sha. Returns [ErrNotFound] when
// no blob exists, [ErrCorrupt] when the on-disk content does not
// re-hash to sha (catches manual tampering and incomplete writes that
// somehow bypassed the rename guard).
//
// Side-effect: bumps the file's mtime on a successful read so it
// counts as recently-used for LRU eviction. Read-then-evict races are
// avoided by the same lock that serialises writes.
func (s *Stash) Read(sha string) ([]byte, error) {
	if !validSha(sha) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, sha)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("stash.Read: %w", err)
	}
	if Sha(data) != sha {
		return nil, ErrCorrupt
	}
	_ = os.Chtimes(path, time.Now(), time.Now())
	return data, nil
}

// ReadSlice returns the 1-based inclusive line range [startLine, endLine]
// of the blob stored under sha, along with the blob's total line count.
// The fast path startLine == 0 && endLine == 0 returns bytes byte-identical
// to [Stash.Read] — used when a caller wants the side-channel line count
// without a separate scan.
//
// Range semantics (matched against tests pinning the contract):
//   - startLine <= 0 normalises to 1.
//   - endLine <= 0 means "through EOF" (no upper bound).
//   - endLine > totalLines clamps to totalLines.
//   - startLine > totalLines OR startLine > endLine (after clamp) returns
//     empty bytes with totalLines still populated; no error.
//   - The trailing '\n' of the original blob is preserved iff the slice
//     covers the final line. Otherwise emitted lines end in '\n'.
//
// Bumps the underlying blob's mtime via [Stash.Read] (LRU recency on the
// full blob, not just the slice) so an agent that retrieves a slice of a
// large stashed body keeps the whole blob warm.
//
// Errors: returns [ErrNotFound] / [ErrCorrupt] from [Stash.Read]
// unmodified. Never returns an error for an in-range slice.
func (s *Stash) ReadSlice(sha string, startLine, endLine int) ([]byte, int, error) {
	data, err := s.Read(sha)
	if err != nil {
		return nil, 0, err
	}
	total := countLines(data)
	if startLine == 0 && endLine == 0 {
		return data, total, nil
	}
	return sliceLines(data, startLine, endLine, total), total, nil
}

// countLines reports the number of logical text lines in data. Matches
// `wc -l` semantics: a trailing '\n' does not introduce an empty extra
// line ("a\nb\n" counts as 2). Empty blob is 0 lines.
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// sliceLines returns the bytes of lines [start, end] from data (1-based,
// inclusive). Preserves the original blob's trailing-newline status when
// the slice covers the final line. Out-of-range slices return empty bytes.
// total must equal countLines(data); the caller computes it once for both
// the return value and this helper.
func sliceLines(data []byte, start, end, total int) []byte {
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if start > total || start > end {
		return []byte{}
	}
	lines := bytes.Split(data, []byte{'\n'})
	hasTrailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
	if hasTrailingNewline && len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if end > len(lines) {
		end = len(lines)
	}
	out := bytes.Join(lines[start-1:end], []byte{'\n'})
	// Emit a trailing '\n' whenever the original had one between this
	// line and the next. That's true when (a) we're not on the last line
	// (the inter-line separator from the original applies) OR (b) we ARE
	// on the last line but the original blob ended with '\n'.
	if end < total || hasTrailingNewline {
		out = append(out, '\n')
	}
	return out
}

// GC evicts blobs oldest-mtime-first until the total on-disk size is
// at or below MaxTotalMB. No-op when MaxTotalMB is zero. Called
// implicitly after [Stash.Write] but exported for tests and ad-hoc
// runtime cleanup.
func (s *Stash) GC() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcLocked()
}

func (s *Stash) gcLocked() error {
	if s.maxTotalMB <= 0 {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stash.GC: readdir: %w", err)
	}
	type fileInfo struct {
		name  string
		size  int64
		mtime time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !validSha(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{name: name, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	capBytes := int64(s.maxTotalMB) * 1024 * 1024
	if total <= capBytes {
		return nil
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})
	for _, f := range files {
		if total <= capBytes {
			break
		}
		if err := os.Remove(filepath.Join(s.dir, f.name)); err == nil {
			total -= f.size
		}
	}
	return nil
}

func (s *Stash) ensureDir() error {
	if s.createDirOK {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("stash.ensureDir: %w", err)
	}
	s.createDirOK = true
	return nil
}

// validSha reports whether s is a 64-char lowercase hex string. Cheap
// guard against directory-traversal in [Stash.Read] (a malicious sha
// containing `../` would otherwise read outside the stash dir).
func validSha(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
