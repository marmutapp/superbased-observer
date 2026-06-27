package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SessionMessages returns the captured native-OTel content bodies (prompts /
// tool input / tool output) for one session — the substrate for the audited
// message-content viewer (Teams dashboard Phase 7). found is false when no
// session with that id is in the caller's scope (→ the handler returns 404,
// which doubles as the out-of-scope response so existence is not disclosed),
// exactly like SessionDetail.
//
// This is a server-side READ of data the node ALREADY shipped under its own
// full_content / admin_managed opt-in (otel_content, agent migration 048 /
// server 007); it changes no wire shape. Scope: admin → all; lead → their
// teams' members; plain member → self. Content is the agent-scrubbed body and
// is present only where the node shared it (hash-only otherwise). Project
// identity is the content-free hash-derived id; the raw path is never selected.
func SessionMessages(ctx context.Context, db *sql.DB, id string, scope Scope, selfUserID string, now time.Time) (MessagesResult, bool, error) {
	uScope, uArgs := peopleScopeSQL("user_id", scope, selfUserID)
	if uScope == falseScope {
		return MessagesResult{}, false, nil
	}

	// Existence/scope probe mirrors SessionDetail: the session must be visible
	// in the caller's scope via the sessions table, so "out of scope" and
	// "unknown" are indistinguishable (no existence leak). This also yields the
	// owning user_id (to constrain the body load) and the project hash.
	var res MessagesResult
	var hash string
	//nolint:gosec // G201: uScope is a parameterized fragment; id + scope args bind via ?.
	metaQ := `SELECT id, user_id, COALESCE(project_root_hash,'') FROM sessions WHERE id = ? AND ` + uScope + ` LIMIT 1`
	row := db.QueryRowContext(ctx, metaQ, append([]any{id}, uArgs...)...)
	if err := row.Scan(&res.SessionID, &res.UserID, &hash); err != nil {
		if err == sql.ErrNoRows {
			return MessagesResult{}, false, nil
		}
		return MessagesResult{}, false, fmt.Errorf("rollup.SessionMessages: meta: %w", err)
	}
	if hash != "" {
		res.ProjectID = ProjectIDFromHash(hash)
	}
	res.Messages = []MessageEntry{}

	// Identity from the authoritative SCIM member store.
	if email, name, err := lookupIdentity(ctx, db, res.UserID); err != nil {
		return MessagesResult{}, false, fmt.Errorf("rollup.SessionMessages: identity: %w", err)
	} else {
		res.Email, res.DisplayName = email, name
	}

	// Body load: the OTel content rows for this session, constrained to the
	// owning user (defence in depth) and ordered chronologically. content is
	// NULL unless the node shared full content — we SELECT it but never any
	// attribution column (user_email / pushed_by_user_id) into the result.
	q := `
SELECT COALESCE(kind,''), COALESCE(request_id,''), COALESCE(tool_use_id,''),
       COALESCE(timestamp,''), content, COALESCE(content_hash,'')
  FROM otel_content
 WHERE session_id = ? AND user_id = ?
 ORDER BY COALESCE(timestamp,''), request_id, tool_use_id, rowid`
	if err := eachRow(ctx, db, q, []any{id, res.UserID}, func(rows *sql.Rows) error {
		var m MessageEntry
		var content sql.NullString
		if err := rows.Scan(&m.Kind, &m.RequestID, &m.ToolUseID, &m.Timestamp, &content, &m.ContentHash); err != nil {
			return err
		}
		if content.Valid && content.String != "" {
			m.Content = content.String
			res.ContentAvailable = true
		}
		res.Messages = append(res.Messages, m)
		return nil
	}); err != nil {
		return MessagesResult{}, false, fmt.Errorf("rollup.SessionMessages: bodies: %w", err)
	}

	return res, true, nil
}
