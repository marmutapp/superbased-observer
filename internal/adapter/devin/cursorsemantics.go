package devin

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics. `sessions.db`
// and its `-wal` / `-shm` sidecars are scanned by a
// `message_nodes.row_id` watermark, so the persisted cursor is a row
// id rather than a byte offset.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	return adapter.FileCursorSemantics{
		Kind:   adapter.CursorWatermark,
		Detail: "devin sessions.db is scanned by a message_nodes.row_id watermark; the cursor is not a byte offset",
	}
}
