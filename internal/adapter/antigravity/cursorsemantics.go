package antigravity

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics.
//
// Antigravity `.pb` conversation files (desktop AND the older CLI
// layout) are OSCrypt-encrypted. On a host where the secret cannot be
// retrieved or the cipher mode is unknown (documented for Windows),
// the adapter marks the file unrecoverable and advances the cursor to
// the file size — leaving a cursor-at-EOF row with zero actions
// forever. That is the DESIGNED outcome of an undecodable store, not
// the adapter-misroute fingerprint, so it must not be reported as one.
//
// Byte lag stays meaningful: the cursor really is a file size, and a
// grown `.pb` genuinely awaits another decrypt attempt.
//
// The newer CLI plaintext-protobuf SQLite `.db` store is parsed
// directly and keeps ordinary byte-offset semantics.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	switch classifyLayout(path) {
	case LayoutDesktop, LayoutCLI:
		return adapter.FileCursorSemantics{
			Kind:   adapter.CursorEncrypted,
			Detail: "antigravity .pb conversations are OSCrypt-encrypted; emitting no actions is expected wherever the secret or cipher is unavailable on this host",
		}
	default:
		return adapter.FileCursorSemantics{}
	}
}
