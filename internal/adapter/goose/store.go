package goose

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/platform/sqlitedsn"
)

// allHomesFunc is the test seam over crossmount.AllHomes — tests override
// it to assert foreign-mount detection without depending on the host's
// filesystem layout. Same shape as devin.allHomesFunc / crush.allHomesFunc.
var allHomesFunc = crossmount.AllHomes

// storeSubdir returns the path segments of goose's session store directory
// for a home following the given logical OS. goose uses XDG on Linux/macOS
// (~/.local/share/goose/sessions) and %APPDATA%\Block\goose\data\sessions
// on Windows.
func storeSubdir(homeOS string) []string {
	if homeOS == crossmount.OSWindows {
		return []string{"AppData", "Roaming", "Block", "goose", "data", "sessions"}
	}
	return []string{".local", "share", "goose", "sessions"}
}

// defaultRoots returns one watch root per cross-mount-resolved home: the
// goose `sessions` directory. The OS is the LOGICAL OS of the home
// (crossmount.HomeRoot.OS), not runtime.GOOS — so a WSL2 observer reaches
// a Windows-side store at
// /mnt/c/Users/<u>/AppData/Roaming/Block/goose/data/sessions and a Windows
// observer reaches a WSL store at
// \\wsl.localhost\<distro>\home\<u>\.local\share\goose\sessions.
// Non-existent dirs are inert (the watcher skips them at registry time).
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
		return nil, fmt.Errorf("goose.stageMirror: %w", err)
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
// foreign-mount source (e.g. /mnt/c/Users/<u>/AppData/Roaming/Block/goose/
// data/sessions/sessions.db read by a WSL2 observer) it stages a local
// mirror of the SQLite trio (.db + -wal + -shm) into a per-source cache
// dir and returns the mirrored .db path. modernc.org/sqlite hits
// SQLITE_IOERR against /mnt/c paths while goose holds the WAL open; the
// mirror reads bytes once via os.ReadFile (which DOES work) then opens the
// in-tmp copy. Same pattern crush / devin / kirocli adopt.
func stageMirrorIfForeign(srcDB string) (string, error) {
	if !isForeignMountPath(srcDB) {
		return srcDB, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		cache = os.TempDir()
	}
	sum := sha256.Sum256([]byte(srcDB))
	mirrorDir := filepath.Join(cache, "superbased-observer", "goose-mirror", hex.EncodeToString(sum[:8]))
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

// maxMessageID returns the largest messages.id in the store — the
// incremental watermark. A missing table (foreign/partial schema) yields 0.
func maxMessageID(ctx context.Context, db *sql.DB) (int64, error) {
	if !tableExists(ctx, db, "messages") {
		return 0, nil
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(id) FROM messages`).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// loadTouchedSessions returns the sessions that gained at least one message
// with id > fromOffset, each with the metadata + accumulated token bundle
// needed to emit its events and a session-level TokenEvent.
func loadTouchedSessions(ctx context.Context, db *sql.DB, fromOffset int64) ([]sessionRow, error) {
	if !tableExists(ctx, db, "sessions") || !tableExists(ctx, db, "messages") {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, COALESCE(s.working_dir, ''), COALESCE(s.model_config_json, ''),
		       COALESCE(s.provider_name, ''), COALESCE(s.updated_at, ''),
		       s.accumulated_input_tokens, s.accumulated_output_tokens,
		       s.accumulated_cache_read_tokens, s.accumulated_cache_write_tokens,
		       s.accumulated_total_tokens, s.accumulated_cost
		  FROM sessions s
		 WHERE EXISTS (
		       SELECT 1 FROM messages m
		        WHERE m.session_id = s.id AND m.id > ?)
		 ORDER BY s.updated_at ASC, s.id ASC`, fromOffset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		if err := rows.Scan(&s.ID, &s.WorkingDir, &s.ModelConfigJSON, &s.Provider, &s.UpdatedAt,
			&s.AccInput, &s.AccOutput, &s.AccCacheRead, &s.AccCacheWrite,
			&s.AccTotal, &s.AccCost); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadMessages returns every message of a session in chronological (id
// ASC) order.
func loadMessages(ctx context.Context, db *sql.DB, sessionID string) ([]messageRow, error) {
	if !tableExists(ctx, db, "messages") {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(message_id, ''), role, content_json, created_timestamp
		  FROM messages
		 WHERE session_id = ?
		 ORDER BY id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []messageRow
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.ID, &m.MessageID, &m.Role, &m.ContentJSON, &m.Created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// tableExists reports whether a table is present in the store, so a foreign
// or older schema degrades gracefully instead of erroring.
func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
	return err == nil && got == name
}
