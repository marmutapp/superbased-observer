package kilocode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/adapter/cline"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// LegacyAdapter watches the legacy Kilo Code IDE extension
// (`kilocode.kilo-code`) which is a Cline + Roo Code fork. The
// per-task storage layout — `<globalStorage>/kilocode.kilo-code/
// tasks/<taskId>/api_conversation_history.json` (+ `ui_messages.json`)
// — is BYTE-IDENTICAL to Cline, so we reuse the existing
// internal/adapter/cline parser by wrapping a cline.Adapter with our
// own watch roots and re-tagging every emitted event with
// Tool = models.ToolKiloCode.
//
// The choice of wrapping rather than factoring the cline parser into a
// shared helper avoids touching the cline package's public surface and
// keeps the cline adapter's regression suite untouched as the
// authoritative cookbook for the format.
type LegacyAdapter struct {
	scrubber   *scrub.Scrubber
	watchRoots []string
	delegate   *cline.Adapter
}

// NewLegacy returns a LegacyAdapter with default scrubber and the
// canonical Kilo Code globalStorage paths under every cross-mount-
// resolved $HOME (covers WSL2 reading Windows-side VS Code installs
// and vice-versa).
func NewLegacy() *LegacyAdapter {
	a := &LegacyAdapter{scrubber: scrub.New()}
	a.watchRoots = a.defaultRoots()
	a.delegate = cline.NewWithOptions(a.scrubber, a.watchRoots)
	return a
}

// NewLegacyWithOptions customizes scrubber and/or watch roots (used by
// tests to point at a t.TempDir() rather than the developer's real
// globalStorage).
func NewLegacyWithOptions(s *scrub.Scrubber, watchRoots []string) *LegacyAdapter {
	if s == nil {
		s = scrub.New()
	}
	a := &LegacyAdapter{scrubber: s, watchRoots: watchRoots}
	if len(a.watchRoots) == 0 {
		a.watchRoots = a.defaultRoots()
	}
	a.delegate = cline.NewWithOptions(s, a.watchRoots)
	return a
}

// Name implements adapter.Adapter.
func (*LegacyAdapter) Name() string { return models.ToolKiloCode }

// WatchPaths implements adapter.Adapter.
func (a *LegacyAdapter) WatchPaths() []string { return a.watchRoots }

// IsSessionFile implements adapter.Adapter. Matches the same basename
// as Cline (`api_conversation_history.json`) but constrained to paths
// under THIS adapter's `kilocode.kilo-code/tasks/` roots — preserves
// the v1.4.51 dispatch contract so a foreign-rooted file with the
// same basename can't be silently claimed.
func (a *LegacyAdapter) IsSessionFile(path string) bool {
	if filepath.Base(path) != "api_conversation_history.json" {
		return false
	}
	return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
}

// ParseSessionFile implements adapter.Adapter by delegating to the
// wrapped cline adapter and re-tagging every emitted event with
// Tool = models.ToolKiloCode. The format is identical between the two
// products; only the dashboard-facing attribution differs.
func (a *LegacyAdapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	res, err := a.delegate.ParseSessionFile(ctx, path, fromOffset)
	if err != nil {
		return res, err
	}
	for i := range res.ToolEvents {
		res.ToolEvents[i].Tool = models.ToolKiloCode
	}
	for i := range res.TokenEvents {
		res.TokenEvents[i].Tool = models.ToolKiloCode
	}
	return res, nil
}

// defaultRoots returns the canonical `kilocode.kilo-code/tasks/` paths
// under every cross-mount-resolved $HOME's VS Code globalStorage.
// Mirrors the cline adapter's per-OS branching exactly so an observer
// running under WSL2 reaches Kilo data living at
// /mnt/c/Users/<u>/AppData/Roaming/Code/User/globalStorage and a
// Windows-side observer reaches a WSL distro's Code-server install.
//
// Remote-WSL (VS Code Server) is covered by also globbing
// `~/.vscode-server/data/User/globalStorage` per home — without this,
// sessions captured by VS Code's Remote-WSL extension never appear.
func (a *LegacyAdapter) defaultRoots() []string {
	var roots []string
	for _, h := range crossmount.AllHomes() {
		base := vsCodeGlobalStorage(h)
		if base != "" {
			roots = append(roots, filepath.Join(base, "kilocode.kilo-code", "tasks"))
		}
		// VS Code Server (Remote-WSL / SSH / Dev Containers) puts
		// extension globalStorage under ~/.vscode-server/data/User/...
		// — distinct from the desktop globalStorage covered above.
		if remote := vsCodeServerGlobalStorage(h); remote != "" {
			roots = append(roots, filepath.Join(remote, "kilocode.kilo-code", "tasks"))
		}
	}
	return roots
}

// vsCodeGlobalStorage returns the VS Code desktop globalStorage
// subpath under the given cross-mount-resolved $HOME, branching on
// the home's LOGICAL OS. Duplicated from
// internal/adapter/cline/adapter.go::vsCodeGlobalStorage — the cline
// version is package-private. Keeping a copy here avoids exporting a
// helper that's only meaningful to two adapters.
func vsCodeGlobalStorage(h crossmount.HomeRoot) string {
	switch h.OS {
	case crossmount.OSWindows:
		if h.Origin == "native" && runtime.GOOS == "windows" {
			if appData := os.Getenv("APPDATA"); appData != "" {
				return filepath.Join(appData, "Code", "User", "globalStorage")
			}
		}
		return filepath.Join(h.Path, "AppData", "Roaming", "Code", "User", "globalStorage")
	case crossmount.OSDarwin:
		return filepath.Join(h.Path, "Library", "Application Support", "Code", "User", "globalStorage")
	case crossmount.OSLinux:
		return filepath.Join(h.Path, ".config", "Code", "User", "globalStorage")
	}
	return ""
}

// vsCodeServerGlobalStorage returns the VS Code Server's per-user
// globalStorage subpath. VS Code Server only runs on POSIX-shaped
// homes (Linux distros under WSL, SSH remotes, Dev Container
// userspaces) so we restrict to OSLinux + OSDarwin. Returns "" for
// Windows homes — VS Code desktop runs natively there, not Server.
func vsCodeServerGlobalStorage(h crossmount.HomeRoot) string {
	if h.OS == crossmount.OSLinux || h.OS == crossmount.OSDarwin {
		return filepath.Join(h.Path, ".vscode-server", "data", "User", "globalStorage")
	}
	return ""
}
