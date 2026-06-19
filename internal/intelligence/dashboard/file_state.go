package dashboard

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// handleFileState serves GET /api/file/state?path=<abs>.
//
// Drives the VS Code extension's FileDecorationProvider + HoverProvider
// (M5 — docs/vscode-extension-tracker.md): a small dot + Markdown
// hover on files the operator's AI tools touched recently.
//
// Response shape:
//
//	{
//	  "path": "/abs/path/to/file",
//	  "last_read_at": "RFC3339",
//	  "last_read_by": "claude-code",
//	  "edit_count_24h": 12,
//	  "stale_rereads_24h": 3,
//	  "tools_touched": ["claude-code","cursor"]
//	}
//
// Notes:
//
//   - `target` is the action's path field as written by the adapter;
//     it matches `?path=` verbatim — the caller passes the absolute
//     path it sees in the editor.
//   - `stale_rereads_24h` counts reads that the freshness engine
//     flagged as `'stale'` in the last 24 h. We deliberately name it
//     "observed" rather than the plan's "avoided" because the reads
//     still happen (the AI tool doesn't read observer's mind); the
//     count is the observability signal, not the prevention count.
//   - Empty path → 400; missing data → 200 with zero-valued fields
//     (the extension treats "no rows" as "no decoration" without
//     surfacing an error to the user).
func (s *Server) handleFileState(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, `missing required query argument "path"`, http.StatusBadRequest)
		return
	}

	cutoff := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)

	state := fileStateResponse{Path: path, ToolsTouched: []string{}}

	// 1. last_read_at + last_read_by — the most recent read across
	//    any session/tool.
	if err := s.db().QueryRowContext(
		r.Context(),
		`SELECT a.timestamp, COALESCE(s.tool, '')
		   FROM actions a
		   JOIN sessions s ON s.id = a.session_id
		  WHERE a.target = ?
		    AND a.action_type = 'read_file'
		  ORDER BY a.timestamp DESC
		  LIMIT 1`,
		path,
	).Scan(&state.LastReadAt, &state.LastReadBy); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, fmt.Errorf("file_state: last_read: %w", err))
		return
	}

	// 2. edit_count_24h — edit_file + write_file in the window.
	if err := s.db().QueryRowContext(
		r.Context(),
		`SELECT COUNT(*)
		   FROM actions
		  WHERE target = ?
		    AND action_type IN ('edit_file', 'write_file')
		    AND timestamp >= ?`,
		path, cutoff,
	).Scan(&state.EditCount24h); err != nil {
		writeErr(w, fmt.Errorf("file_state: edit_count_24h: %w", err))
		return
	}

	// 3. stale_rereads_24h — reads tagged 'stale' in the window.
	if err := s.db().QueryRowContext(
		r.Context(),
		`SELECT COUNT(*)
		   FROM actions
		  WHERE target = ?
		    AND action_type = 'read_file'
		    AND freshness = 'stale'
		    AND timestamp >= ?`,
		path, cutoff,
	).Scan(&state.StaleRereads24h); err != nil {
		writeErr(w, fmt.Errorf("file_state: stale_rereads_24h: %w", err))
		return
	}

	// 4. tools_touched — distinct tools that hit this path in the
	//    last 24h. Stable sort so the response is byte-deterministic.
	rows, err := s.db().QueryContext(
		r.Context(),
		`SELECT DISTINCT s.tool
		   FROM actions a
		   JOIN sessions s ON s.id = a.session_id
		  WHERE a.target = ?
		    AND a.timestamp >= ?
		    AND s.tool != ''
		  ORDER BY s.tool`,
		path, cutoff,
	)
	if err != nil {
		writeErr(w, fmt.Errorf("file_state: tools_touched: %w", err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			writeErr(w, fmt.Errorf("file_state: tools_touched scan: %w", err))
			return
		}
		state.ToolsTouched = append(state.ToolsTouched, tool)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, fmt.Errorf("file_state: tools_touched iter: %w", err))
		return
	}

	writeJSON(w, state)
}

// fileStateResponse is the on-the-wire shape returned by
// handleFileState. Kept inside the dashboard package so the JSON tags
// stay coupled to the handler.
type fileStateResponse struct {
	Path            string   `json:"path"`
	LastReadAt      string   `json:"last_read_at"`
	LastReadBy      string   `json:"last_read_by"`
	EditCount24h    int      `json:"edit_count_24h"`
	StaleRereads24h int      `json:"stale_rereads_24h"`
	ToolsTouched    []string `json:"tools_touched"`
}
