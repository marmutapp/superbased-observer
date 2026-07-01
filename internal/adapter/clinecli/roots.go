package clinecli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// defaultRoots returns the Cline CLI home directory under every
// cross-mount-resolved $HOME. Cline CLI uses `<home>/.cline` on every
// OS (NOT %LOCALAPPDATA% on Windows — confirmed against a live install).
// This is simpler than the Hermes adapter's defaultRoots (no OS
// branching) but still walks the crossmount surface so a WSL2 observer
// reaches data living at `/mnt/c/Users/<u>/.cline/`.
//
// Respects CLINE_DIR when set on the running observer (rare — usually
// only set by the user inside their CLI shell). When set, CLINE_DIR
// adds to the per-home discovery on the running process's native side;
// cross-mount candidates from other OSes still contribute their default
// `<home>/.cline` paths. Mirrors how the Cline CLI's @cline/shared
// storage resolver reads the env.
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

	// CLINE_DIR wins on the native side when set.
	if env := strings.TrimSpace(os.Getenv("CLINE_DIR")); env != "" {
		add(env)
	}

	for _, h := range crossmount.AllHomes() {
		if h.Path == "" {
			continue
		}
		// Same layout on every OS — no branching.
		add(filepath.Join(h.Path, ".cline"))
	}
	return roots
}
