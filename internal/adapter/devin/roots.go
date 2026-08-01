package devin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/adapter/mirrorbase"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/platform/sqlitedsn"
)

// allHomesFunc is the test seam over crossmount.AllHomes — tests override
// it to assert foreign-mount detection without depending on the host's
// filesystem layout. Same shape as kirocli.allHomesFunc / crush.allHomesFunc.
var allHomesFunc = crossmount.AllHomes

// storeSubdir returns the path segments of Devin's CLI store directory for
// a home following the given logical OS. Devin uses XDG on Linux/macOS
// (~/.local/share/devin/cli) and %APPDATA% (AppData\Roaming\devin\cli) on
// Windows.
func storeSubdir(homeOS string) []string {
	if homeOS == crossmount.OSWindows {
		return []string{"AppData", "Roaming", "devin", "cli"}
	}
	return []string{".local", "share", "devin", "cli"}
}

// defaultRoots returns one watch root per cross-mount-resolved home: the
// Devin CLI store directory. The OS is the LOGICAL OS of the home
// (crossmount.HomeRoot.OS), not runtime.GOOS — so a WSL2 observer reaches
// a Windows-side store at /mnt/c/Users/<u>/AppData/Roaming/devin/cli and a
// Windows observer reaches a WSL store at
// \\wsl.localhost\<distro>\home\<u>\.local\share\devin\cli. Non-existent
// dirs are inert (the watcher skips them at registry time).
func defaultRoots() []string {
	var roots []string
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		roots = append(roots, p)
	}
	for _, h := range allHomesFunc() {
		if h.Path == "" {
			continue
		}
		add(filepath.Join(append([]string{h.Path}, storeSubdir(h.OS)...)...))
	}
	return roots
}

// resolveDBPath maps a -wal/-shm sibling event back to the main
// sessions.db.
func resolveDBPath(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if base == "sessions.db-wal" || base == "sessions.db-shm" {
		return filepath.Join(filepath.Dir(path), "sessions.db")
	}
	return path
}

// openReadOnlyDB opens the sessions.db read-only, staging a local mirror
// first when the source is a foreign mount.
func openReadOnlyDB(path string) (*sql.DB, error) {
	actual, err := stageMirrorIfForeign(path)
	if err != nil {
		return nil, fmt.Errorf("devin.stageMirror: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)", sqlitedsn.Escape(actual))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// stageMirrorIfForeign returns srcDB unchanged when it's native. For a
// foreign-mount source (e.g. /mnt/c/Users/<u>/AppData/Roaming/devin/cli/
// sessions.db read by a WSL2 observer) it stages a local mirror of the
// SQLite trio (.db + -wal + -shm) into a per-source cache dir and returns
// the mirrored .db path. modernc.org/sqlite hits SQLITE_IOERR against
// /mnt/c paths while Devin holds the WAL open; the mirror reads bytes once
// via os.ReadFile (which DOES work) then opens the in-tmp copy. Same
// pattern crush / kirocli / clinecli adopt.
func stageMirrorIfForeign(srcDB string) (string, error) {
	if !isForeignMountPath(srcDB) {
		return srcDB, nil
	}
	base, err := mirrorbase.Base()
	if err != nil || base == "" {
		base = filepath.Join(os.TempDir(), "superbased-observer")
	}
	sum := sha256.Sum256([]byte(srcDB))
	mirrorDir := filepath.Join(base, "devin-mirror", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(mirrorDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir mirror: %w", err)
	}
	dstDB := filepath.Join(mirrorDir, "sessions.db")
	if mirrorUpToDate(srcDB, dstDB) {
		return dstDB, nil
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := srcDB + suffix
		dst := dstDB + suffix
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				_ = os.Remove(dst)
				continue
			}
			return "", fmt.Errorf("read %s: %w", src, err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return "", fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return dstDB, nil
}

// mirrorUpToDate reports whether the mirror trio is at least as fresh as
// the source, using (size, mtime) per sibling. WAL is the fast-moving
// signal, so it is checked explicitly. Any stat error returns false so a
// fresh copy is attempted.
func mirrorUpToDate(srcDB, dstDB string) bool {
	if !filesMatch(srcDB, dstDB) {
		return false
	}
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

// isForeignMountPath reports whether path lives under a crossmount-detected
// non-native home. Covers both bridge directions (/mnt/c on WSL2, and
// \\wsl.localhost\ on Windows).
func isForeignMountPath(path string) bool {
	for _, h := range allHomesFunc() {
		if h.Origin == "native" {
			continue
		}
		sep := string(filepath.Separator)
		if strings.HasPrefix(path, h.Path+sep) || strings.HasPrefix(path, h.Path+"/") {
			return true
		}
	}
	return false
}
