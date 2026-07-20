package kirocli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter is the file-watcher implementation of the Kiro CLI adapter.
// It parses BOTH on-disk layouts of the mode-dependent dual store (see
// the package doc): the interactive flat-file bundle under
// `~/.kiro/sessions/cli/` and the non-interactive SQLite
// `conversations_v2` table under the kiro-cli data dir. Layout is
// resolved from the file shape at IsSessionFile / ParseSessionFile
// time — one package, layout-sniffing dispatch, mirroring antigravity.
type Adapter struct {
	scrubber *scrub.Scrubber
	roots    []string
}

// New returns an Adapter with platform-default cross-mount roots and a
// default scrubber.
func New() *Adapter {
	return &Adapter{
		scrubber: scrub.New(),
		roots:    defaultRoots(),
	}
}

// NewWithOptions customises scrubber and roots for tests. Pass nil
// scrubber for the default; pass no roots for default platform
// discovery.
func NewWithOptions(s *scrub.Scrubber, roots ...string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	return &Adapter{scrubber: s, roots: roots}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolKiroCLI }

// WatchPaths implements adapter.Adapter.
func (a *Adapter) WatchPaths() []string { return a.roots }

// layout identifies which Kiro CLI store shape a path belongs to.
type layout int

const (
	layoutUnknown layout = iota
	// layoutFlat is a flat-bundle `<uuid>.json` or `<uuid>.jsonl` under
	// `.../.kiro/sessions/cli/`. Both extensions route to the same
	// bundle parse and emit events under the canonical `.jsonl`
	// SourceFile so the store's (source_file, source_event_id) dedup
	// drops the cross-trigger duplicates.
	layoutFlat
	// layoutSQLite is the `data.sqlite3` (or its -wal/-shm sidecar)
	// under a kiro-cli data dir. The sidecars route here purely as
	// fsnotify triggers; the open always targets data.sqlite3.
	layoutSQLite
)

// classifyLayout returns the store layout for a path, gated on the
// path living under a recognisable Kiro subtree. Root-gating in
// IsSessionFile is still required — this only recognises the shape.
func classifyLayout(path string) layout {
	// Normalise separators first so a Windows path read on Linux (where
	// filepath.Base won't split on backslashes) still yields the right
	// basename.
	norm := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	base := norm
	if i := strings.LastIndex(norm, "/"); i >= 0 {
		base = norm[i+1:]
	}

	switch base {
	case "data.sqlite3", "data.sqlite3-wal", "data.sqlite3-shm":
		// Belt-and-braces: parent dir must look like a kiro-cli data
		// dir on either OS layout.
		if strings.Contains(norm, "/kiro-cli/") {
			return layoutSQLite
		}
		return layoutUnknown
	}

	if strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".jsonl") {
		if strings.Contains(norm, "/.kiro/sessions/cli/") {
			return layoutFlat
		}
	}
	return layoutUnknown
}

// IsSessionFile implements adapter.Adapter. Two families, each ANDed
// with adapter.UnderAnyWatchRoot so a stray data.sqlite3 / <uuid>.json
// elsewhere on disk cannot claim the dispatch:
//
//  1. Flat bundle: `<uuid>.json` / `<uuid>.jsonl` under
//     `.../.kiro/sessions/cli/`. The `.history` / `.lock` siblings are
//     rejected (read only as bundle siblings, never as triggers).
//  2. SQLite: `data.sqlite3` (+ -wal / -shm poll triggers) under the
//     kiro-cli data dir.
func (a *Adapter) IsSessionFile(path string) bool {
	if classifyLayout(path) == layoutUnknown {
		return false
	}
	return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
}

// ParseSessionFile implements adapter.Adapter, dispatching on layout.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	switch classifyLayout(path) {
	case layoutFlat:
		return a.parseFlatBundle(ctx, path, fromOffset)
	case layoutSQLite:
		return a.parseStateDB(ctx, path, fromOffset)
	default:
		return adapter.ParseResult{NewOffset: fromOffset}, nil
	}
}

// bundlePaths derives the canonical `.jsonl` + `.json` sibling paths
// for a flat-bundle trigger (either extension).
func bundlePaths(trigger string) (jsonlPath, jsonPath, sessionID string) {
	dir := filepath.Dir(trigger)
	base := filepath.Base(trigger)
	sessionID = strings.TrimSuffix(strings.TrimSuffix(base, ".jsonl"), ".json")
	jsonlPath = filepath.Join(dir, sessionID+".jsonl")
	jsonPath = filepath.Join(dir, sessionID+".json")
	return jsonlPath, jsonPath, sessionID
}

// warnf appends a formatted warning to res.Warnings.
func warnf(res *adapter.ParseResult, format string, args ...any) {
	res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...))
}
