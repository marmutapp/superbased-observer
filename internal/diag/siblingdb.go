package diag

import (
	"os"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// SiblingDB is a cross-environment observer.db the running daemon will
// not read directly.
type SiblingDB struct {
	Path   string // absolute path to the foreign-home observer.db
	Origin string // crossmount HomeRoot.Origin, e.g. "wsl-mnt:marmu"
	OS     string // crossmount HomeRoot.OS, e.g. "windows"
}

// DetectCrossEnvSiblingDBs returns observer.db files in FOREIGN-OS homes
// (Windows homes seen from WSL, or Linux homes seen from Windows) that
// differ from the daemon's own dbPath. These hold data the daemon won't
// read directly — cross-OS hook capture is handled by registering the AI
// tool's hooks as a wsl.exe bridge so they run in the daemon's context,
// but rows written by a stale native-binary registration (writing the
// foreign DB) or by a separate daemon pointed at it stay stranded.
// Callers WARN so the split is visible rather than silent.
//
// Native-home candidates are intentionally excluded so a custom daemon
// dbPath never false-positives its own native default location; only the
// cross-environment Windows<->WSL straddle is flagged. A candidate equal
// to dbPath (the daemon's own file, reached via a foreign mount alias) is
// also skipped.
func DetectCrossEnvSiblingDBs(dbPath string, homes []crossmount.HomeRoot) []SiblingDB {
	want := filepath.Clean(dbPath)
	var out []SiblingDB
	for _, h := range homes {
		if h.Origin == "native" {
			continue
		}
		cand := filepath.Join(h.Path, ".observer", "observer.db")
		if filepath.Clean(cand) == want {
			continue
		}
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() {
			continue
		}
		out = append(out, SiblingDB{Path: cand, Origin: h.Origin, OS: h.OS})
	}
	return out
}
