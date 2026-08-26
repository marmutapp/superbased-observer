package zcode

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics. `db.sqlite`
// and its `-wal` sidecar are scanned by a `MAX(time_updated)`
// watermark, so the persisted cursor is a timestamp rather than a byte
// offset.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	return adapter.FileCursorSemantics{
		Kind:   adapter.CursorWatermark,
		Detail: "db.sqlite is scanned by a MAX(time_updated) watermark; the cursor is not a byte offset",
	}
}
