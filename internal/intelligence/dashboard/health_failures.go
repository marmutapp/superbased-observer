package dashboard

import (
	"net/http"
	"strconv"
	"time"
)

// Failures surface (usability arc P4.11 / review §8.1): the
// failure_context table has carried recovered-vs-not per failed
// command since v1, with zero UI. One endpoint, grouped the way the
// operator thinks — "what command keeps failing, did it ever pass,
// where do I look" — rendered as a card on the Health section.

// failureGroup is one command's failure history in the window.
type failureGroup struct {
	Command       string `json:"command"`
	Fails         int64  `json:"fails"`
	Retries       int64  `json:"retries"`
	Recovered     bool   `json:"recovered"`
	LastAt        string `json:"last_at"`
	ErrorCategory string `json:"error_category,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	SessionID     string `json:"session_id"`
	Project       string `json:"project,omitempty"`
}

// handleHealthFailures serves GET /api/health/failures?days=N
// (default 7, max 30). Groups by command_hash; the bare columns ride
// SQLite's documented MAX()-row semantics, so error/session/project
// come from each group's most recent failure.
func (s *Server) handleHealthFailures(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 30 {
			days = n
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)

	groups := []failureGroup{}
	var total int64
	if s.db() != nil {
		_ = s.db().QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM failure_context WHERE timestamp >= ?`, cutoff).Scan(&total)
		rows, err := s.db().QueryContext(r.Context(),
			`SELECT f.command_summary, COUNT(*), COALESCE(SUM(f.retry_count), 0),
			        MAX(f.eventually_succeeded), MAX(f.timestamp),
			        COALESCE(f.error_category, ''), COALESCE(f.error_message, ''),
			        f.session_id, COALESCE(p.name, '')
			 FROM failure_context f
			 LEFT JOIN projects p ON p.id = f.project_id
			 WHERE f.timestamp >= ?
			 GROUP BY f.command_hash
			 ORDER BY MAX(f.timestamp) DESC
			 LIMIT 50`, cutoff)
		if err != nil {
			writeErr(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var g failureGroup
			var recovered int64
			if rows.Scan(&g.Command, &g.Fails, &g.Retries, &recovered, &g.LastAt,
				&g.ErrorCategory, &g.ErrorMessage, &g.SessionID, &g.Project) == nil {
				g.Recovered = recovered > 0
				groups = append(groups, g)
			}
		}
		_ = rows.Err()
	}

	writeJSON(w, map[string]any{
		"window_days": days,
		"total":       total,
		"failures":    groups,
	})
}
