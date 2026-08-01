package kilocode

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics for the Kilo
// CLI store. `kilo.db` and its `-wal` sidecar are scanned by a
// `MAX(time_updated)` watermark, so the persisted cursor is a
// timestamp rather than a byte offset.
//
// The legacy IDE adapter (LegacyAdapter) wraps the Cline extension
// parser, which tails append-only JSON files by byte offset — it
// deliberately does NOT implement this interface.
func (a *CLIAdapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	return adapter.FileCursorSemantics{
		Kind:   adapter.CursorWatermark,
		Detail: "kilo.db is scanned by a MAX(time_updated) watermark; the cursor is not a byte offset",
	}
}
