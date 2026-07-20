package dashboard

import "net/http"

// handleTerminalRuns serves GET /api/terminal/runs (View): the read-only
// terminal-run history (plan §E), metadata only (hashes/coords, never paths or
// command text), newest first, each folded with its strongest correlated
// session + observed command-boundary count. Node-local — no push-path change.
func (s *Server) handleTerminalRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := s.remoteManageStore()
	if st == nil {
		writeJSON(w, map[string]any{"runs": []any{}})
		return
	}
	limit := intArg(r, "limit", 50, 1, 500)
	runs, err := st.ListTerminalRuns(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	type row struct {
		RunID          string  `json:"run_id"`
		Tool           string  `json:"tool"`
		Kind           string  `json:"kind"`
		LaunchedAt     string  `json:"launched_at"`
		EndedAt        string  `json:"ended_at,omitempty"`
		ExitCode       *int    `json:"exit_code,omitempty"`
		Running        bool    `json:"running"`
		BestSessionID  string  `json:"best_session_id,omitempty"`
		BestConfidence float64 `json:"best_confidence,omitempty"`
		CommandCount   int     `json:"command_count"`
	}
	out := make([]row, 0, len(runs))
	for _, rn := range runs {
		item := row{
			RunID:          rn.RunID,
			Tool:           rn.Tool,
			Kind:           rn.Kind,
			LaunchedAt:     rn.LaunchedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			ExitCode:       rn.ExitCode,
			Running:        rn.EndedAt == nil,
			BestSessionID:  rn.BestSessionID,
			BestConfidence: rn.BestConfidence,
			CommandCount:   rn.CommandCount,
		}
		if rn.EndedAt != nil {
			item.EndedAt = rn.EndedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, item)
	}
	writeJSON(w, map[string]any{"runs": out})
}
