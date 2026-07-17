package hermes

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// defaultRoots returns the Hermes home directory under every cross-mount-
// resolved $HOME. The location varies by OS convention:
//
//   - Linux / macOS / WSL2: `<home>/.hermes`
//   - Windows native:       `<home>/AppData/Local/hermes`
//
// On WSL2 the bridge surfaces both the Linux native home (a `.hermes`
// candidate) and one or more Windows homes under /mnt/c/Users/<u> (each
// becomes an `AppData/Local/hermes` candidate). The watcher's
// Detected() filter prunes directories that don't actually exist on
// disk, so unused candidates cost nothing.
//
// Respects HERMES_HOME when set on the running observer (rare — usually
// only set by the user inside their hermes shell, not the observer's).
// When set, HERMES_HOME REPLACES the per-home discovery on the running
// process's native side; cross-mount candidates from other OSes still
// contribute their default paths. This mirrors how the Hermes Python
// loader reads the env var.
func defaultRoots() []string {
	var roots []string
	seen := map[string]struct{}{}
	add := func(p string) {
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		roots = append(roots, p)
	}

	// HERMES_HOME wins for the native side when set.
	if env := strings.TrimSpace(os.Getenv("HERMES_HOME")); env != "" {
		add(env)
	}

	for _, h := range crossmount.AllHomes() {
		if h.Path == "" {
			continue
		}
		switch h.OS {
		case crossmount.OSWindows:
			add(filepath.Join(h.Path, "AppData", "Local", "hermes"))
		default:
			// linux + darwin both use ~/.hermes per the Hermes install docs.
			add(filepath.Join(h.Path, ".hermes"))
		}
	}
	return roots
}
