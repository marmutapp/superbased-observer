package clinecli

import (
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// CursorSemanticsFor implements adapter.CursorSemantics. cline-cli
// mixes two cursor shapes, which is why the interface is per-path:
//
//   - `sessions.db` (+ `-wal` / `-shm` fsnotify sidecars) is scanned by
//     a UnixMilli `sessions.updated_at` watermark. That watermark is
//     ~1.8e12 while the file is ~1e6 bytes, so comparing it to the file
//     size reports the store as permanently caught-up-with-zero-actions
//     — the misroute fingerprint, inverted into a false positive.
//   - `hooks.jsonl` is a genuine append-only byte-offset tail.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	switch filepath.Base(path) {
	case "sessions.db", "sessions.db-wal", "sessions.db-shm":
		return adapter.FileCursorSemantics{
			Kind:   adapter.CursorWatermark,
			Detail: "cline-cli sessions.db is scanned by a UnixMilli sessions.updated_at watermark; the cursor is not a byte offset",
		}
	default:
		return adapter.FileCursorSemantics{}
	}
}
