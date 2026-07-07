package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Audit action codes (stable machine strings written to audit_log.action).
const (
	ActionViewDevelopers = "view_team_developers"  // GET .../developers
	ActionDrillDown      = "drill_down_developers" // explicit "show breakdown" click
	ActionRevokeBearer   = "revoke_bearer"
	ActionSetTeamRole    = "set_team_role"
	// ActionViewGuardAgents records the per-agent chain-continuity
	// disclosure (GET /api/org/guard/agents) — per-developer rows, so the
	// developers-drill-down audit rule applies (guard spec §14.3, G14).
	ActionViewGuardAgents = "view_guard_agents"
	// ActionPublishBundle records a dashboard policy-bundle publish; the
	// published version rides target_detail (guard spec §14.5, G14).
	ActionPublishBundle = "publish_policy_bundle"
	// ActionViewPeople records the org-wide per-developer leaderboard
	// disclosure (GET /api/org/people) — per-developer rows, so the same
	// developers-drill-down audit rule applies. No team target (it spans the
	// whole scope), so target_team_id is empty.
	ActionViewPeople = "view_org_developers"
	// ActionViewSessions records the org Sessions disclosure (GET
	// /api/org/sessions and .../{id}) — each row/detail names a developer, the
	// same disclosure class as People. target_detail carries the session id on
	// a detail view, empty for a list.
	ActionViewSessions = "view_org_sessions"
	// ActionViewSessionMessages records the org message-content disclosure (GET
	// /api/org/sessions/{id}/messages) — the captured OTel bodies (prompts /
	// tool input/output) for one session. This is a DEEPER disclosure than the
	// metadata-only ActionViewSessions: reading the actual prose is its own
	// recorded act, so it gets a distinct, louder action string. target_detail
	// carries the session id.
	ActionViewSessionMessages = "view_session_messages"
	// ActionViewSpanContent records the org trajectory span-content disclosure
	// (GET /api/org/obs/trace/{id}/content) — the raw prompt/response/tool-io
	// bodies for one trace's spans (obs-org-tier T3). Like
	// ActionViewSessionMessages this is a DEEPER disclosure than the metadata
	// span tree, so it gets its own distinct, louder action string;
	// target_detail carries the trace id.
	ActionViewSpanContent = "view_span_content"
)

// WriteAudit appends one audit_log row. It is the single writer for the table,
// called by the dashboard handlers before a privacy-sensitive disclosure
// (developer drill-down) and on admin mutations (revoke, role change).
// targetTeamID/targetDetail/sourceIP may be empty.
func WriteAudit(ctx context.Context, db *sql.DB, actorUserID, action, targetTeamID, targetDetail, sourceIP string, now time.Time) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO audit_log (actor_user_id, action, target_team_id, target_detail, source_ip, timestamp)
VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`,
		actorUserID, action, targetTeamID, targetDetail, sourceIP, now.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("rollup.WriteAudit: %w", err)
	}
	return nil
}

// Audit returns one page of the audit log, newest first. An admin scope sees
// every row; a lead sees only rows targeting a team they lead. limit is clamped
// to [1, 500]; has_more / next_offset support offset pagination.
func Audit(ctx context.Context, db *sql.DB, scope Scope, limit, offset int) (AuditResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	res := AuditResult{Entries: []AuditEntry{}, NextOffset: offset}

	where := "1=1"
	var args []any
	if !scope.Admin {
		if len(scope.TeamIDs) == 0 {
			return res, nil // leads no teams → nothing visible
		}
		where = "a.target_team_id IN (" + placeholders(len(scope.TeamIDs)) + ")"
		for _, t := range scope.TeamIDs {
			args = append(args, t)
		}
	}

	// Fetch one extra row to detect has_more without a second count query.
	//nolint:gosec // G202: SQL structure (SELECT/JOIN) is code-constant and `where` is a parameterized scope fragment; all values are bound via ? args.
	q := `
SELECT a.id, a.actor_user_id, COALESCE(om.email,''), a.action,
       COALESCE(a.target_team_id,''), COALESCE(a.target_detail,''),
       COALESCE(a.source_ip,''), a.timestamp
  FROM audit_log a
  LEFT JOIN org_members om ON om.user_id = a.actor_user_id
 WHERE ` + where + `
 ORDER BY a.id DESC
 LIMIT ? OFFSET ?`
	args = append(args, limit+1, offset)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return AuditResult{}, fmt.Errorf("rollup.Audit: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorEmail, &e.Action,
			&e.TargetTeamID, &e.TargetDetail, &e.SourceIP, &e.Timestamp); err != nil {
			return AuditResult{}, fmt.Errorf("rollup.Audit: scan: %w", err)
		}
		res.Entries = append(res.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return AuditResult{}, err
	}

	if len(res.Entries) > limit {
		res.Entries = res.Entries[:limit]
		res.HasMore = true
		res.NextOffset = offset + limit
	} else {
		res.NextOffset = offset + len(res.Entries)
	}
	return res, nil
}
