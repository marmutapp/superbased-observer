package grok

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics. The global
// `logs/unified.jsonl` is the token-split log correlated back to
// sessions by `sid`; it emits TokenEvents only, so it can never
// accumulate action rows. Per-session `updates.jsonl` files keep
// ordinary byte-offset semantics.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	if isUnifiedLog(path) {
		return adapter.FileCursorSemantics{
			Kind:   adapter.CursorNoActions,
			Detail: "grok logs/unified.jsonl is the global token-split log correlated by sid; it emits token rows only",
		}
	}
	return adapter.FileCursorSemantics{}
}
