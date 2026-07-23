package fsview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultMaxReadBytes is the default per-file read cap Read applies when the
	// caller passes maxBytes <= 0.
	DefaultMaxReadBytes = 256 * 1024
	// maxEntries caps a single directory listing. Entries beyond the cap are
	// dropped (after the dirs-first/name sort) and Truncated is reported true.
	maxEntries = 2000
	// binarySniffBytes is the prefix length scanned for a NUL byte to classify a
	// file as binary.
	binarySniffBytes = 8 * 1024
)

// Sentinel errors. Callers use errors.Is to map them to transport-level
// responses. None of them carry an absolute filesystem path, so a handler may
// surface them without leaking the resolved location.
var (
	// ErrOutsideRoot is returned when a resolved path escapes the project root
	// (via ".." or a symlink pointing outside the tree).
	ErrOutsideRoot = errors.New("fsview: path escapes project root")
	// ErrNotFound is returned when the requested path (or the root) does not
	// exist.
	ErrNotFound = errors.New("fsview: path not found")
	// ErrAbsolutePath is returned when the requested path is absolute; callers
	// must pass project-relative paths only.
	ErrAbsolutePath = errors.New("fsview: path must be project-relative")
	// ErrNotDir is returned by List when the target exists but is not a listable
	// directory (a plain file, or a symlink — symlinked dirs are typed, not
	// listable).
	ErrNotDir = errors.New("fsview: not a directory")
	// ErrIsDir is returned by Read when the target exists but is a directory.
	ErrIsDir = errors.New("fsview: path is a directory")
	// ErrNotRegular is returned by Read when the target exists but is neither a
	// regular file nor a directory (a FIFO, device, or socket). Reading such a
	// node could block an HTTP handler indefinitely, so it is refused.
	ErrNotRegular = errors.New("fsview: not a regular file")
)

// EntryType classifies a directory entry without following symlinks.
type EntryType string

const (
	// EntryFile is a regular file.
	EntryFile EntryType = "file"
	// EntryDir is a directory.
	EntryDir EntryType = "dir"
	// EntrySymlink is a symbolic link (never followed by List).
	EntrySymlink EntryType = "symlink"
	// EntryOther is any other node kind (device, socket, pipe).
	EntryOther EntryType = "other"
)

// Entry is one directory child. Size and ModTime are the entry's own (Lstat)
// values, so a symlink reports the link's metadata, not its target's.
type Entry struct {
	Name    string
	Type    EntryType
	Size    int64
	ModTime time.Time
}

// Content is the result of reading a file. When Binary is true, Data is empty
// (the raw bytes are never returned for a binary file). Truncated is true when
// the file exceeded the read cap and Data holds only the leading maxBytes.
type Content struct {
	Data      string
	Size      int64
	Truncated bool
	Binary    bool
}

// Resolve joins root and rel, resolves symlinks on both, and verifies the
// result is contained under the resolved root. It returns the resolved absolute
// path on success. The ctx guards the filesystem I/O EvalSymlinks performs
// (checked up front; the syscalls themselves are not cancellable). See resolve
// for the full contract.
func Resolve(ctx context.Context, root, rel string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("fsview.Resolve: %w", err)
	}
	real, _, _, err := resolve(root, rel)
	return real, err
}

// resolve is the shared path-resolution core. It returns the fully
// symlink-resolved absolute target (real), the resolved root (rootResolved) so
// callers can re-verify containment immediately before use, and the lexical
// joined path BEFORE the final component's symlink was followed (joined) so List
// can reject a symlinked final component. The rel must be project-relative; an
// absolute rel is rejected with ErrAbsolutePath. Containment is checked AFTER
// EvalSymlinks so a symlink under the tree that points outside it is denied
// (ErrOutsideRoot). A missing root or target yields ErrNotFound.
func resolve(root, rel string) (real, rootResolved, joined string, err error) {
	if root == "" {
		return "", "", "", errors.New("fsview.resolve: root is required")
	}
	if filepath.IsAbs(rel) {
		return "", "", "", ErrAbsolutePath
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", "", fmt.Errorf("fsview.resolve: abs root: %w", err)
	}
	rootResolved, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", "", ErrNotFound
		}
		return "", "", "", fmt.Errorf("fsview.resolve: resolve root symlinks: %w", err)
	}

	// filepath.Join cleans the result, so an embedded ".." collapses here.
	// Check lexical containment BEFORE resolving symlinks so a "..'"-escape is
	// rejected as outside-root regardless of whether the escaped path exists.
	joined = filepath.Join(rootResolved, filepath.FromSlash(rel))
	if !isUnder(joined, rootResolved) {
		return "", "", "", ErrOutsideRoot
	}
	candResolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", "", ErrNotFound
		}
		return "", "", "", fmt.Errorf("fsview.resolve: resolve path symlinks: %w", err)
	}
	// Second containment check catches a symlink UNDER the tree pointing OUT.
	if !isUnder(candResolved, rootResolved) {
		return "", "", "", ErrOutsideRoot
	}
	return candResolved, rootResolved, joined, nil
}

// reverifyContained re-resolves target's symlinks and confirms it is still under
// rootResolved, returning the freshly canonicalized path to actually open. It
// narrows (does not close) the check-then-use TOCTOU window between resolve and
// the Open/ReadDir below by re-running EvalSymlinks immediately before the I/O:
// a final component swapped to an outside-pointing symlink after resolve is
// caught here. The residual window is an accepted risk documented in doc.go.
func reverifyContained(rootResolved, target string) (string, error) {
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("fsview.reverifyContained: resolve symlinks: %w", err)
	}
	if !isUnder(real, rootResolved) {
		return "", ErrOutsideRoot
	}
	return real, nil
}

// isUnder reports whether child equals parent or is contained beneath it. Both
// paths must be cleaned and absolute.
func isUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}

// List returns the children of the directory at rel (relative to root), sorted
// directories-first then by name. It does not follow symlinked directories —
// such entries are reported with Type EntrySymlink and are not themselves
// listable (a rel whose own final component is a symlink is rejected with
// ErrNotDir). The listing is capped at maxEntries; when the cap is hit the extra
// entries are dropped and the returned bool (truncated) is true. ctx is checked
// before the filesystem I/O so a cancelled request does no work.
func List(ctx context.Context, root, rel string) ([]Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	real, rootResolved, joined, err := resolve(root, rel)
	if err != nil {
		return nil, false, err
	}
	// Reject a rel whose OWN final component is a symlink (Lstat does not follow
	// it): symlinked directories are typed EntrySymlink by a parent listing and
	// are not themselves listable. resolve already followed it, so we must Lstat
	// the pre-resolution joined path to see the link.
	if li, lerr := os.Lstat(joined); lerr == nil && li.Mode()&fs.ModeSymlink != 0 {
		return nil, false, ErrNotDir
	}
	info, err := os.Lstat(real)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, ErrNotFound
		}
		return nil, false, fmt.Errorf("fsview.List: lstat: %w", err)
	}
	if !info.IsDir() {
		return nil, false, ErrNotDir
	}
	// Re-verify containment immediately before opening, then read incrementally:
	// f.ReadDir(maxEntries+1) materializes at most cap+1 DirEntry values, so a
	// directory with millions of children cannot exhaust memory (finding 4).
	realDir, err := reverifyContained(rootResolved, real)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(realDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, ErrNotFound
		}
		return nil, false, fmt.Errorf("fsview.List: open dir: %w", err)
	}
	defer func() { _ = f.Close() }()
	des, err := f.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("fsview.List: read dir: %w", err)
	}
	truncated := false
	if len(des) > maxEntries {
		des = des[:maxEntries]
		truncated = true
	}
	entries := make([]Entry, 0, len(des))
	for _, d := range des {
		e := Entry{Name: d.Name(), Type: entryType(d.Type())}
		if fi, ierr := d.Info(); ierr == nil {
			e.Size = fi.Size()
			e.ModTime = fi.ModTime()
		}
		entries = append(entries, e)
	}
	// Sort only the retained set (finding 4): dirs first, then by name.
	sort.Slice(entries, func(i, j int) bool {
		di := entries[i].Type == EntryDir
		dj := entries[j].Type == EntryDir
		if di != dj {
			return di
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, truncated, nil
}

// entryType maps an fs.FileMode (from a non-following DirEntry.Type) to an
// EntryType. Symlinks are classified as symlinks regardless of their target.
func entryType(m fs.FileMode) EntryType {
	switch {
	case m&fs.ModeSymlink != 0:
		return EntrySymlink
	case m.IsDir():
		return EntryDir
	case m.IsRegular():
		return EntryFile
	default:
		return EntryOther
	}
}

// Read returns the contents of the file at rel (relative to root), capped at
// maxBytes (DefaultMaxReadBytes when maxBytes <= 0). A file whose first
// binarySniffBytes contain a NUL byte is reported Binary with empty Data. A
// file larger than the cap is reported Truncated with only the leading maxBytes
// in Data. A directory target yields ErrIsDir; a FIFO/device/socket yields
// ErrNotRegular (it could otherwise block the handler indefinitely). ctx is
// checked before the filesystem I/O so a cancelled request does no work.
func Read(ctx context.Context, root, rel string, maxBytes int) (Content, error) {
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxReadBytes
	}
	real, rootResolved, _, err := resolve(root, rel)
	if err != nil {
		return Content{}, err
	}
	info, err := os.Lstat(real)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Content{}, ErrNotFound
		}
		return Content{}, fmt.Errorf("fsview.Read: lstat: %w", err)
	}
	// Require a regular file BEFORE opening (finding 3): a directory is ErrIsDir,
	// and a FIFO/device/socket is ErrNotRegular so os.Open cannot hang the
	// handler waiting for a writer.
	switch {
	case info.IsDir():
		return Content{}, ErrIsDir
	case !info.Mode().IsRegular():
		return Content{}, ErrNotRegular
	}
	// Re-verify containment immediately before opening (narrows the swap-to-
	// symlink TOCTOU window; see doc.go).
	realFile, err := reverifyContained(rootResolved, real)
	if err != nil {
		return Content{}, err
	}
	// openForRead opens non-blocking on unix so a regular file swapped to a FIFO
	// between Lstat and open returns immediately instead of blocking; the
	// post-open IsRegular check below then rejects it.
	f, err := openForRead(realFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Content{}, ErrNotFound
		}
		return Content{}, fmt.Errorf("fsview.Read: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Re-check on the OPEN descriptor: closes the Lstat→open swap race.
	fi, err := f.Stat()
	if err != nil {
		return Content{}, fmt.Errorf("fsview.Read: fstat: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Content{}, ErrNotRegular
	}

	// Read one byte past the cap so truncation is detected independently of the
	// stat size (the file can grow between Lstat and read).
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return Content{}, fmt.Errorf("fsview.Read: read: %w", err)
	}
	truncated := false
	if len(data) > maxBytes {
		truncated = true
		data = data[:maxBytes]
	}
	sniff := data
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	c := Content{Size: fi.Size(), Truncated: truncated}
	if bytes.IndexByte(sniff, 0) >= 0 {
		c.Binary = true
		return c, nil
	}
	c.Data = string(data)
	return c, nil
}
