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

// SiblingObserver is a coarse descriptor of a second observer.db on this
// machine that the managed daemon does not own — tamper-EVIDENCE (Arc 4 P6b,
// plan §9) of a parallel/bypass observer. It carries ONLY the crossmount origin
// and OS labels, never a filesystem path, because it feeds the org
// managed-integrity wire whose content-floor forbids paths/usernames.
type SiblingObserver struct {
	Origin string // coarse crossmount origin label, e.g. "wsl-mnt:marmu" or "native-alt"
	OS     string
}

// DetectSiblingObservers returns coarse descriptors of observer.db files on
// this machine that differ from the daemon's own dbPath. It generalises
// DetectCrossEnvSiblingDBs from a local start.go WARN into the fleet-signal
// input the managed-integrity probe reports to the org:
//
//   - the cross-OS Windows↔WSL straddles DetectCrossEnvSiblingDBs already finds,
//     re-labelled to drop the path; PLUS
//   - a native-home default-location observer.db that differs from a CUSTOM
//     daemon dbPath (labelled "native-alt"). This fires ONLY when the daemon
//     runs a non-default dbPath — an MDM-pinned managed node — and a
//     default-location DB also exists, i.e. a likely parallel default install.
//     When the daemon uses the default path (the common case) the candidate IS
//     dbPath and is skipped, so there is no false positive.
//
// EVIDENCE, not proof: a determined developer on a machine they control can
// place or hide a DB anywhere; this catches the ordinary cases and feeds the
// admin a signal, it is not a security boundary (§5 MDM gate is the lock).
func DetectSiblingObservers(dbPath string, homes []crossmount.HomeRoot) []SiblingObserver {
	var out []SiblingObserver
	for _, s := range DetectCrossEnvSiblingDBs(dbPath, homes) {
		out = append(out, SiblingObserver{Origin: s.Origin, OS: s.OS})
	}
	want := filepath.Clean(dbPath)
	for _, h := range homes {
		if h.Origin != "native" {
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
		out = append(out, SiblingObserver{Origin: "native-alt", OS: h.OS})
	}
	return out
}
