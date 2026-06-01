package adapter

import (
	"path/filepath"
	"runtime"
	"strings"
)

// caseInsensitiveFS toggles the case-folded path comparison used by
// HasPathPrefix on filesystems that are case-insensitive by default
// (Windows + macOS HFS+/APFS). On Linux paths stay case-sensitive.
//
// Exposed as a var so tests can flip it to exercise both branches
// regardless of host platform.
var caseInsensitiveFS = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// HasPathPrefix returns true when prefix is equal to path or is a
// strict ancestor directory of path. Both arguments are normalized
// via filepath.Abs + filepath.Clean before comparison.
//
// On case-insensitive filesystems (Windows, macOS) the comparison is
// case-folded. On Linux it is exact.
//
// Symlinks are NOT resolved here. fsnotify reports events under the
// symlinked path and parse_cursors stores them the same way, so a
// symlinked watch root and the files it owns agree at lookup time
// without an extra EvalSymlinks call per check. Out-of-band
// parse_cursors rows that hold a resolved-target path while
// WatchPaths returns the symlink path will fail this check — a known
// limitation, revisit if it bites.
func HasPathPrefix(path, prefix string) bool {
	p, ok := normalizePath(path)
	if !ok {
		return false
	}
	pre, ok := normalizePath(prefix)
	if !ok {
		return false
	}
	if caseInsensitiveFS {
		p = strings.ToLower(p)
		pre = strings.ToLower(pre)
	}
	if p == pre {
		return true
	}
	if !strings.HasPrefix(p, pre) {
		return false
	}
	return p[len(pre)] == filepath.Separator
}

// UnderAnyWatchRoot returns true when path is equal to or nested
// under at least one of roots. Used by every adapter's IsSessionFile
// to scope shape-based predicates to the adapter's own watch roots.
//
// Background: until v1.4.51 the watcher's poll fallback dispatched
// files to adapters via the first-IsSessionFile-match rule with the
// adapter registry sorted alphabetically. Combined with broad
// predicates (claude-code's `.jsonl` extension only), a Codex
// rollout-*.jsonl file under ~/.codex/sessions matched claude-code's
// predicate first and was silently parsed by the wrong adapter,
// stranding token rows whenever fsnotify dropped a write event on
// WSL2/NTFS. The fix is two-layered:
//
//  1. Watcher.adapterFor now dispatches via longest-watched-root
//     prefix (root-based, never shape-based).
//  2. Every adapter's IsSessionFile ANDs its shape predicate with
//     UnderAnyWatchRoot(a.WatchPaths()) — predicates self-limit to
//     paths the adapter could actually own.
//
// Either layer alone would prevent the bug; both together make the
// bug-class regression-proof.
func UnderAnyWatchRoot(path string, roots []string) bool {
	for _, r := range roots {
		if r == "" {
			continue
		}
		if HasPathPrefix(path, r) {
			return true
		}
	}
	return false
}

func normalizePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	return filepath.Clean(abs), true
}
