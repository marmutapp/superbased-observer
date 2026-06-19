package clinecli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// allHomesFunc is the test seam over crossmount.AllHomes — tests
// override it to assert foreign-mount detection without depending on
// the host's filesystem layout. Same shape as opencode.allHomesFunc.
var allHomesFunc = crossmount.AllHomes

// stageMirrorIfForeign returns srcDB unchanged when it's native. For
// foreign-mount sources (e.g. `/mnt/c/Users/<u>/.cline/data/db/sessions.db`
// on a WSL2 Linux host reading the Windows-side Cline CLI store) it
// stages a local mirror — copying the SQLite trio (.db + -wal + -shm)
// into a per-source cache dir and returning the path to the mirrored
// .db. SQLite open + reads against the mirror succeed where the
// foreign-mount path fails with SQLITE_IOERR_SEEK (4618).
//
// Why the mirror is load-bearing on /mnt/c: modernc.org/sqlite returns
// disk-I/O errors against the foreign-mount sessions.db while the
// Windows cline process holds the file open in WAL mode. WSL's
// DrvFs bridge translates the file descriptor into something SQLite's
// pread can't seek through; the mirror avoids the bridge entirely by
// reading bytes once via os.ReadFile (which DOES work) and then
// opening the in-tmp copy. Same pattern opencode adopted for
// Windows-side OpenCode Desktop captures.
//
// Refresh policy: skip the copy when every source file is older than
// (or matches) the mirror sibling. WAL is the fast-moving signal —
// its mtime advances faster than the main .db as Cline appends turn
// data. Same applies for the reverse direction (Windows host reading
// `\\wsl.localhost\<distro>\…\sessions.db` would hit the same race
// on a sufficiently loaded WSL writer).
//
// Mirror lives at `<UserCacheDir>/superbased-observer/clinecli-mirror/<hash>/`
// where `<hash>` is the first 8 bytes of SHA-256(srcDB) hex-encoded.
// Per-source so concurrent WSL + Windows installs don't trample each
// other's mirrors.
func stageMirrorIfForeign(srcDB string) (string, error) {
	if !isForeignMountPath(srcDB) {
		return srcDB, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		cache = os.TempDir()
	}
	sum := sha256.Sum256([]byte(srcDB))
	mirrorDir := filepath.Join(cache, "superbased-observer", "clinecli-mirror", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(mirrorDir, 0o700); err != nil {
		return "", fmt.Errorf("clinecli.stageMirror: mkdir %s: %w", mirrorDir, err)
	}
	dstDB := filepath.Join(mirrorDir, "sessions.db")
	if mirrorUpToDate(srcDB, dstDB) {
		return dstDB, nil
	}
	// Copy the trio. Main .db is the primary; -wal carries pages the
	// writer hasn't checkpointed yet; -shm is the shared-memory index
	// of the WAL (32 KB, regenerable but matching siblings keeps SQLite
	// from re-deriving it on open). Missing siblings are removed from
	// the mirror so a stale -wal doesn't shadow a freshly-checkpointed
	// source.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := srcDB + suffix
		dst := dstDB + suffix
		data, err := os.ReadFile(src) //nolint:gosec // src derives from validated watch roots
		if err != nil {
			if os.IsNotExist(err) {
				_ = os.Remove(dst)
				continue
			}
			return "", fmt.Errorf("clinecli.stageMirror: read %s: %w", src, err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return "", fmt.Errorf("clinecli.stageMirror: write %s: %w", dst, err)
		}
	}
	return dstDB, nil
}

// mirrorUpToDate reports whether the mirror trio is at least as fresh
// as the source trio. Uses (size, mtime) per sibling — the size guard
// catches an in-flight truncate/realloc that mtime alone misses
// (occasionally seen on /mnt/c when Windows checkpoints the WAL).
// Returns false on any stat error so a fresh copy gets attempted.
func mirrorUpToDate(srcDB, dstDB string) bool {
	if !filesMatch(srcDB, dstDB) {
		return false
	}
	// WAL is the fast-moving signal — its mtime ticks every flush.
	if sw, err := os.Stat(srcDB + "-wal"); err == nil {
		if !filesMatchInfo(sw, dstDB+"-wal") {
			return false
		}
	}
	return true
}

func filesMatch(src, dst string) bool {
	s, err := os.Stat(src)
	if err != nil {
		return false
	}
	return filesMatchInfo(s, dst)
}

func filesMatchInfo(srcInfo os.FileInfo, dst string) bool {
	d, err := os.Stat(dst)
	if err != nil {
		return false
	}
	if srcInfo.Size() != d.Size() {
		return false
	}
	return !srcInfo.ModTime().After(d.ModTime())
}

// isForeignMountPath reports whether path lives under a
// crossmount-detected non-native home. Both directions are covered:
// `/mnt/c/Users/<u>/…` on a WSL2 Linux host, and
// `\\wsl.localhost\<distro>\home\<u>\…` on a Windows host. The
// watcher dispatches both native and foreign-mount homes into the
// same adapter; only the foreign ones need the mirror — native
// paths open directly with no overhead.
func isForeignMountPath(path string) bool {
	for _, h := range allHomesFunc() {
		if h.Origin == "native" {
			continue
		}
		// Accept both the OS-native separator and forward slash —
		// /mnt/c uses forward slashes regardless of the foreign OS.
		sep := string(filepath.Separator)
		if strings.HasPrefix(path, h.Path+sep) || strings.HasPrefix(path, h.Path+"/") {
			return true
		}
	}
	return false
}
