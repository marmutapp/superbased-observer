package dashboard

import (
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
)

// Global search (usability arc P6.2 / review §8.2): the FTS5 index
// behind the MCP search_past_outputs tool, surfaced in the dashboard.
// Same index, same query semantics; hits are enriched with the owning
// action's session/timestamp so the UI can place each result in time
// and deep-link into the session.

// snippetStartMark / snippetEndMark bracket the FTS5 match region in
// each hit's snippet. Non-text sentinels: the client splits on them
// and renders <mark> elements itself — a stored excerpt can never
// smuggle markup through the result.
const (
	snippetStartMark = "\x01"
	snippetEndMark   = "\x02"
)

// searchHitRow is one enriched FTS5 hit.
type searchHitRow struct {
	ActionID     int64   `json:"action_id"`
	SessionID    string  `json:"session_id,omitempty"`
	Timestamp    string  `json:"timestamp,omitempty"`
	Tool         string  `json:"tool,omitempty"`
	ActionType   string  `json:"action_type,omitempty"`
	ToolName     string  `json:"tool_name,omitempty"`
	Target       string  `json:"target,omitempty"`
	Snippet      string  `json:"snippet,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Rank         float64 `json:"rank"`
}

// handleSearch serves GET /api/search?q=<fts5 query>&limit=N (default
// 20, max 50). Empty q is a 400 — the page gates requests client-side,
// so a blank query reaching the server is a caller bug. Unlike the MCP
// tool this does NOT record retrieval signals: those feed the learn
// miner's "what does the AI re-look-up" analysis, and a human browsing
// the dashboard would pollute it.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	limit := intArg(r, "limit", 20, 1, 50)

	hits := []searchHitRow{}
	if s.db() != nil {
		results, err := indexing.New(s.db(), 0).SearchWithSnippets(
			r.Context(), q, limit, snippetStartMark, snippetEndMark,
		)
		if err != nil {
			// FTS5 syntax errors are user input, not server faults —
			// report as 400 with the parser's message so the page can
			// show "fix your query" instead of a generic failure.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, res := range results {
			hit := searchHitRow{
				ActionID:     res.ActionID,
				ToolName:     res.ToolName,
				Target:       res.Target,
				Snippet:      res.Snippet,
				ErrorMessage: res.ErrorMessage,
				Rank:         res.Rank,
			}
			// Best-effort enrichment; a vanished action row (pruned
			// since indexing) leaves the hit bare rather than dropping
			// it — the excerpt content is still useful.
			_ = s.db().QueryRowContext(r.Context(),
				`SELECT session_id, timestamp, tool, action_type FROM actions WHERE id = ?`,
				res.ActionID).Scan(&hit.SessionID, &hit.Timestamp, &hit.Tool, &hit.ActionType)
			hits = append(hits, hit)
		}
	}

	writeJSON(w, map[string]any{
		"query": q,
		"count": len(hits),
		"hits":  hits,
	})
}
