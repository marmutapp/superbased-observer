package aider

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// Discovery bounds. Aider has no project index, so New() finds watch roots
// by walking each cross-mount home for .aider.chat.history.md files. The
// walk is bounded on BOTH depth and total directories visited so a large
// home does not turn daemon startup into a full-disk crawl.
const (
	// maxWalkDepth caps how many directory levels below a home the walk
	// descends. A repo at ~/code/acme/service is depth 3.
	maxWalkDepth = 8
	// maxDirsVisited caps the total directories examined across all
	// homes; the walk stops early once it is hit.
	maxDirsVisited = 60000
	// maxRoots caps how many distinct repo roots are returned.
	maxRoots = 512
)

// allHomesFunc is the test seam over crossmount.AllHomes.
var allHomesFunc = crossmount.AllHomes

// skipDirNames are directory names never worth descending into when
// hunting for an aider transcript: heavy build/vendor/cache trees that
// never hold a repo root, plus dot-directories (an aider repo root is a
// normal, non-hidden project directory). Compared case-insensitively.
var skipDirNames = map[string]bool{
	"node_modules":  true,
	"vendor":        true,
	"dist":          true,
	"build":         true,
	"target":        true,
	"__pycache__":   true,
	"site-packages": true,
	".git":          true,
}

// discoverRoots builds the watch-root set. Roots are the transcript FILE
// paths themselves (not repo directories): AIDER_CHAT_HISTORY_FILE when
// the env override is set, plus every .aider.chat.history.md found by a
// bounded walk of the NATIVE home. File-roots keep aider's watch set
// disjoint from any project-local adapter watching a subdir of the same
// repo (e.g. crush's <repo>/.crush) and avoid recursively watching whole
// repo trees.
//
// The set is a SNAPSHOT — the walk runs once here. A repo that gains its
// first aider session after the daemon starts is not covered until a
// restart or an `observer scan`/`observer backfill`; see
// Adapter.WatchPaths and docs/aider-adapter.md.
func discoverRoots() []string {
	var roots []string
	seen := map[string]bool{}
	add := func(p string) bool {
		if p == "" || seen[p] {
			return true
		}
		if len(roots) >= maxRoots {
			return false
		}
		seen[p] = true
		roots = append(roots, p)
		return true
	}

	if f := strings.TrimSpace(os.Getenv("AIDER_CHAT_HISTORY_FILE")); f != "" {
		add(filepath.Clean(f))
	}

	budget := maxDirsVisited
	for _, h := range allHomesFunc() {
		if budget <= 0 || len(roots) >= maxRoots {
			break
		}
		// Foreign cross-mount homes are SKIPPED: walking a Windows home
		// over DrvFs/9P measured in MINUTES (vs ~1s native), which is an
		// unacceptable startup and test-suite cost. Windows-side aider
		// repos are the Windows daemon's job; a WSL daemon can still
		// reach one via AIDER_CHAT_HISTORY_FILE or injected roots.
		if h.Origin != "native" {
			continue
		}
		found, spent := walkForAiderRoots(h.Path, budget)
		budget -= spent
		for _, d := range found {
			if !add(d) {
				break
			}
		}
	}
	return roots
}

// walkForAiderRoots walks root BREADTH-FIRST to a bounded depth,
// returning every .aider.chat.history.md file path found and the number
// of directories visited (charged against the caller's budget).
// Breadth-first matters: aider transcripts live at repo roots, which sit
// SHALLOW under a home (~depth 1–4), while a lexical depth-first walk can
// exhaust the whole budget inside one deep tree that sorts early (e.g.
// ~/go/pkg/mod) and never reach a repo that sorts later. Heavy/hidden
// directories are pruned. Best-effort: I/O errors on individual entries
// are skipped, never fatal.
func walkForAiderRoots(root string, budget int) (found []string, visited int) {
	root = filepath.Clean(root)
	seen := map[string]bool{}

	type qdir struct {
		path  string
		depth int
	}
	queue := []qdir{{root, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited >= budget {
			break
		}
		visited++
		entries, err := os.ReadDir(cur.path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				// Prune dot-dirs (an aider repo root is non-hidden) and
				// the heavy build/vendor trees.
				if strings.HasPrefix(name, ".") || skipDirNames[strings.ToLower(name)] {
					continue
				}
				if cur.depth+1 <= maxWalkDepth {
					queue = append(queue, qdir{filepath.Join(cur.path, name), cur.depth + 1})
				}
				continue
			}
			if strings.EqualFold(name, historyFileName) {
				p := filepath.Join(cur.path, name)
				if !seen[p] {
					seen[p] = true
					found = append(found, p)
				}
			}
		}
	}
	return found, visited
}
