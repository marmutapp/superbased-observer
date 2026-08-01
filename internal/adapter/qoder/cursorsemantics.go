package qoder

import (
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// CursorSemanticsFor implements adapter.CursorSemantics. Qoder's
// `logs/sessions/**/segments/*.jsonl` run logs contribute TokenEvents
// from model.response.completed records only — and in live capture
// those records are all-zero and therefore skipped — so a segment file
// legitimately produces nothing. Transcript files keep ordinary
// byte-offset semantics.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	if isSegmentPath(strings.ReplaceAll(strings.ToLower(path), `\`, "/")) {
		return adapter.FileCursorSemantics{
			Kind:   adapter.CursorNoActions,
			Detail: "qoder run-log segments carry token records only; they emit no action rows",
		}
	}
	return adapter.FileCursorSemantics{}
}
