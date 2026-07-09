package crush

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// projectsFile is the shape of ~/.local/share/crush/projects.json — the
// discovery seam that maps every known Crush project to its data dir.
type projectsFile struct {
	Projects []struct {
		Path    string `json:"path"`
		DataDir string `json:"data_dir"`
	} `json:"projects"`
}

// discoverRoots builds the watch-root set: the Crush global state dirs
// across every cross-mount-resolved home, plus every project data dir
// (<project>/.crush) enumerated from each home's projects.json. Foreign
// (Windows) project paths are translated to their /mnt/c equivalent so the
// watcher and IsSessionFile agree on the path form.
//
// The set is a SNAPSHOT — projects.json is read once here. A Crush project
// created after the daemon starts is not covered until a restart or an
// `observer backfill`; see Adapter.WatchPaths.
func discoverRoots() []string {
	var roots []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, p)
	}

	for _, h := range crossmount.AllHomes() {
		for _, stateDir := range stateDirsForHome(h) {
			add(stateDir)
			for _, dataDir := range readProjectDataDirs(filepath.Join(stateDir, "projects.json")) {
				add(dataDir)
			}
		}
	}
	return roots
}

// stateDirsForHome returns the Crush global-state directory candidates for
// one home. Crush follows XDG on Linux/macOS (~/.local/share/crush) and
// %LOCALAPPDATA%\crush on Windows; the macOS Application Support path is
// emitted too for installs that use it. Non-existent dirs are inert (the
// watcher skips them).
func stateDirsForHome(h crossmount.HomeRoot) []string {
	switch h.OS {
	case crossmount.OSWindows:
		return []string{filepath.Join(h.Path, "AppData", "Local", "crush")}
	case crossmount.OSDarwin:
		return []string{
			filepath.Join(h.Path, ".local", "share", "crush"),
			filepath.Join(h.Path, "Library", "Application Support", "crush"),
		}
	default:
		return []string{filepath.Join(h.Path, ".local", "share", "crush")}
	}
}

// readProjectDataDirs parses a projects.json and returns each project's
// data dir, translated for cross-mount access. Best-effort: a missing or
// malformed file yields nil.
func readProjectDataDirs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pf projectsFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil
	}
	var out []string
	for _, p := range pf.Projects {
		dir := p.DataDir
		if dir == "" && p.Path != "" {
			dir = filepath.Join(p.Path, ".crush")
		}
		if dir == "" {
			continue
		}
		out = append(out, crossmount.TranslateForeignPath(dir))
	}
	return out
}
