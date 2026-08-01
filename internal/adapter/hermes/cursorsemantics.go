package hermes

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics. Every file
// this adapter claims (`state.db` and its `-wal` / `-shm` fsnotify
// sidecars) is scanned by a `messages.id` AUTOINCREMENT watermark, so
// the persisted cursor is a row id — never a byte count into the
// multi-megabyte SQLite store.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	return adapter.FileCursorSemantics{
		Kind:   adapter.CursorWatermark,
		Detail: "hermes state.db is scanned by a messages.id row-id watermark; the cursor is not a byte offset",
	}
}
