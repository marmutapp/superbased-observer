package copilotcli

import "github.com/marmutapp/superbased-observer/internal/adapter"

// CursorSemanticsFor implements adapter.CursorSemantics. Copilot CLI's
// `logs/process-*.log` files are scanned for per-Request-ID token
// usage only; the activity itself comes from the paired
// `session-state/<uuid>/events.jsonl`, which keeps ordinary
// byte-offset semantics.
func (a *Adapter) CursorSemanticsFor(path string) adapter.FileCursorSemantics {
	if !a.IsSessionFile(path) {
		return adapter.FileCursorSemantics{}
	}
	if isLogFile(path) {
		return adapter.FileCursorSemantics{
			Kind:   adapter.CursorNoActions,
			Detail: "copilot-cli process logs are scanned for token usage only; activity comes from the paired events.jsonl",
		}
	}
	return adapter.FileCursorSemantics{}
}
