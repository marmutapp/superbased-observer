package m365copilotanalytics

import (
	"context"
	"database/sql"
	"fmt"
)

// UsageSummary is the SAFE, content-free aggregate rollup surfaced on the org
// (web2) dashboard for M365 Copilot. It carries per-appClass interaction /
// prompt / response counts — NO body content and NO cost (aiInteractionHistory is
// not metered). This is the analogue of copilotanalytics.LoadCostSummary: a plain
// read seam the dashboard handler calls, distinct from the admin-tier content
// read below.
type UsageSummary struct {
	OrgID        string
	ByAppClass   []AppClassUsage
	TotalPrompts float64
	TotalResp    float64
}

// AppClassUsage is one M365 surface's aggregate counts.
type AppClassUsage struct {
	AppClass     string
	Interactions float64
	Prompts      float64
	Responses    float64
}

// LoadUsageSummary aggregates Rail A metrics per appClass for an org (or all orgs
// when orgID is ""). Content-free — only the count metrics are read; the content
// table is never touched here.
func LoadUsageSummary(ctx context.Context, db *sql.DB, orgID string) (UsageSummary, error) {
	out := UsageSummary{OrgID: orgID}
	orgFilter, args := orgPredicate(orgID)

	q := `SELECT app_class,
	             COALESCE(SUM(CASE WHEN metric = ? THEN value END), 0),
	             COALESCE(SUM(CASE WHEN metric = ? THEN value END), 0),
	             COALESCE(SUM(CASE WHEN metric = ? THEN value END), 0)
	        FROM m365_copilot_analytics_daily
	       WHERE surface = ?` + orgFilter + `
	       GROUP BY app_class ORDER BY app_class`
	full := append([]any{MetricInteractions, MetricPrompts, MetricResponses, string(SurfaceGraph)}, args...)

	rows, err := db.QueryContext(ctx, q, full...)
	if err != nil {
		return out, fmt.Errorf("m365copilotanalytics: load usage summary: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var u AppClassUsage
		if err := rows.Scan(&u.AppClass, &u.Interactions, &u.Prompts, &u.Responses); err != nil {
			return out, fmt.Errorf("m365copilotanalytics: scan usage: %w", err)
		}
		out.ByAppClass = append(out.ByAppClass, u)
		out.TotalPrompts += u.Prompts
		out.TotalResp += u.Responses
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("m365copilotanalytics: iterate usage: %w", err)
	}
	return out, nil
}

// InteractionEntry is one Rail A content row for the message-content viewer.
// Content is present ONLY when the caller passed adminScope=true AND the row has
// a stored body; ContentHash (content-free) is always present.
type InteractionEntry struct {
	InteractionID   string
	SessionID       string
	AppClass        string
	InteractionType string
	UserKey         string
	CreatedAt       string
	Content         string // "" unless adminScope AND a stored body
	ContentHash     string
}

// InteractionContent is the result of the admin-tier content read.
type InteractionContent struct {
	SessionID        string
	Entries          []InteractionEntry
	ContentAvailable bool // any entry carried a disclosed body
}

// LoadInteractionContent returns the Rail A prompt/response bodies for one
// M365 Copilot session — the substrate for the ADMIN-TIER message-content viewer,
// mirroring rollup.SessionMessages / the otel_content viewer.
//
// It is content-gated by construction: content is disclosed ONLY when
// adminScope is true (an admin, or a lead within their scope, resolved at the
// handler). When adminScope is false the bodies are withheld (metadata + hash
// only) — the read is still permitted so counts/attribution work, but the prose
// never crosses.
//
// CONTRACT (the caller MUST honour, like the otel_content viewer): before calling
// this with adminScope=true, write a DISTINCT, louder audit row
// (rollup action "view_m365_copilot_content") — reading the actual Copilot
// content is a deeper disclosure than the metadata usage summary and is its own
// recorded act. This package deliberately does not import rollup (rule #1 / #2);
// the handler owns the audit write + the scope resolution and passes the decision
// in as adminScope.
func LoadInteractionContent(ctx context.Context, db *sql.DB, sessionID string, adminScope bool) (InteractionContent, error) {
	out := InteractionContent{SessionID: sessionID, Entries: []InteractionEntry{}}

	q := `SELECT interaction_id, COALESCE(session_id,''), app_class,
	             COALESCE(interaction_type,''), user_key, COALESCE(created_at,''),
	             content, content_hash
	        FROM m365_copilot_content
	       WHERE session_id = ?
	       ORDER BY COALESCE(created_at,''), interaction_id`
	rows, err := db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return out, fmt.Errorf("m365copilotanalytics: load content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e InteractionEntry
		var content sql.NullString
		if err := rows.Scan(&e.InteractionID, &e.SessionID, &e.AppClass, &e.InteractionType,
			&e.UserKey, &e.CreatedAt, &content, &e.ContentHash); err != nil {
			return out, fmt.Errorf("m365copilotanalytics: scan content: %w", err)
		}
		// Admin-tier gate: the stored body is disclosed only to an admin scope.
		if adminScope && content.Valid && content.String != "" {
			e.Content = content.String
			out.ContentAvailable = true
		}
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("m365copilotanalytics: iterate content: %w", err)
	}
	return out, nil
}

// orgPredicate returns an optional " AND org_id = ?" filter + its args.
func orgPredicate(orgID string) (string, []any) {
	if orgID == "" {
		return "", nil
	}
	return " AND org_id = ?", []any{orgID}
}
