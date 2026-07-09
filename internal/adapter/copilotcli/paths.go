package copilotcli

import (
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// candidateRoots returns the two watch directories under a given home:
// session-state (for events.jsonl) and logs (for process-*.log).
//
// Layout is identical across Linux / macOS / Windows for the npm-
// installed Copilot CLI — it always writes to `<home>/.copilot/`
// regardless of OS conventions. WSL2 bridges Windows-side sessions
// at `/mnt/c/Users/<u>/.copilot/` via crossmount.AllHomes; no MSIX
// redirection (the CLI is plain Node, not a packaged app).
//
// Both directories are returned regardless of existence — the watcher
// skips non-existent roots at registration time. That keeps WatchPaths
// static and the IsSessionFile under-WatchPaths constraint
// (Invariant #48) well-defined.
func candidateRoots(h crossmount.HomeRoot) []string {
	base := filepath.Join(h.Path, ".copilot")
	return []string{
		filepath.Join(base, "session-state"),
		filepath.Join(base, "logs"),
	}
}
