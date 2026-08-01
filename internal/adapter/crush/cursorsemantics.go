package crush

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics. `crush.db`
// and its `-wal` sidecar are scanned by a Unix-seconds `updated_at`
// watermark, so the persisted cursor is a timestamp rather than a byte
// offset.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	return adapter.FileCursorSemantics{
		Kind:   adapter.CursorWatermark,
		Detail: "crush.db is scanned by a Unix-seconds updated_at watermark; the cursor is not a byte offset",
	}
}
