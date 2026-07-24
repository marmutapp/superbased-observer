package dashboard

import (
	"net/http"
	"strings"
)

// terminalSessionResp is the GET /api/terminal/session/<token> payload: the
// live terminal launch token's run identity plus its observer-session
// correlation. Correlated is the frontend's single boolean gate — when false,
// SessionID is "" and Confidence 0 (the run has no established link yet;
// correlation is scored and asynchronous). When true, SessionID + Confidence
// carry the strongest link scored so far (MAX-upgrade).
type terminalSessionResp struct {
	RunID      string  `json:"run_id"`
	Kind       string  `json:"kind"`
	Tool       string  `json:"tool"`
	Correlated bool    `json:"correlated"`
	SessionID  string  `json:"session_id"`
	Confidence float64 `json:"confidence"`
}

// handleTerminalSession serves the per-terminal session cockpit (Session
// Cockpit):
//
//	GET /api/terminal/session/<token> → run identity + observer-session link
//
// The browser sends only the token (a live launch handle); the daemon resolves
// it — server-side, from state retained at spawn — to the run's kind/tool and,
// once the daemon has correlated the run, its observer session id + link
// confidence. The resolvable set is exactly {live terminal runs}. GET-only and
// View-tier.
//
// Security ordering mirrors handleTerminalProject exactly:
//  1. nil resolver → 404 (a nil seam IS the disabled state; the endpoint's
//     existence is not even confirmable).
//  2. non-GET → 405 method_not_allowed.
//  3. empty token → 404 unknown_token.
//  4. remote gate BEFORE resolution: a remote-exposed caller without
//     [remote].allow_terminal_view gets an IDENTICAL 403 remote_view_disabled
//     for every token state (known or unknown), so the response can't be used
//     as a token oracle.
//  5. resolver known=false (unknown or exited token) → 404 unknown_token.
//  6. 200 JSON with the run identity + correlation.
func (s *Server) handleTerminalSession(w http.ResponseWriter, r *http.Request) {
	// A nil resolver IS the disabled state (cockpit not wired) — 404 like the
	// other nil-seam terminal surfaces, so the endpoint's existence is not even
	// confirmable.
	if s.opts.SessionResolver == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/terminal/session/")
	token, _, _ := strings.Cut(rest, "/")
	if token == "" {
		writeProjectError(w, http.StatusNotFound, "unknown_token")
		return
	}

	// Remote gate FIRST: run/session identity is at least as sensitive as
	// terminal output, so a remote-exposed caller is refused unless the owner
	// has turned on [remote].allow_terminal_view — the exact model handleLaunchWS
	// and handleTerminalProject use. This runs BEFORE SessionResolver so a
	// remote-gated caller gets an IDENTICAL 403 for every token (known, unknown,
	// correlated, or not) and the response can't be used as a token oracle.
	if remoteExposedFromContext(r.Context()) && !s.allowTerminalView() {
		writeProjectError(w, http.StatusForbidden, "remote_view_disabled")
		return
	}

	link, known := s.opts.SessionResolver(token)
	if !known {
		// Unknown or exited token — indistinguishable by design.
		writeProjectError(w, http.StatusNotFound, "unknown_token")
		return
	}

	writeJSON(w, terminalSessionResp{
		RunID:      link.RunID,
		Kind:       link.Kind,
		Tool:       link.Tool,
		Correlated: link.SessionID != "",
		SessionID:  link.SessionID,
		Confidence: link.Confidence,
	})
}
