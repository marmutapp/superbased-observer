package advisor

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// loadPhase2 fills the Phase-2 fact groups: per-session action mix +
// high-effort counts, cachetrack verdicts, MCP audit counts, and the
// failure_context rollup. All best-effort by shape but loud on real query
// errors (P6 applies at the surface layer, not here).
func loadPhase2(ctx context.Context, db *sql.DB, since string, f *Facts) error {
	idx := map[string]*SessionFacts{}
	for i := range f.Sessions {
		idx[f.Sessions[i].ID] = &f.Sessions[i]
	}

	// Action mix + effort. json_extract on the nullable metadata blob —
	// NULL-safe (returns NULL → not counted).
	mixQ := `SELECT session_id,
	       COUNT(*),
	       SUM(CASE WHEN action_type IN ('read_file','search_text','search_files') THEN 1 ELSE 0 END),
	       SUM(CASE WHEN action_type IN ('edit_file','write_file') THEN 1 ELSE 0 END),
	       SUM(CASE WHEN json_extract(metadata,'$.effort_level') IN ('xhigh','max') THEN 1 ELSE 0 END)
	FROM actions WHERE timestamp >= ? GROUP BY session_id`
	rows, err := db.QueryContext(ctx, mixQ, since)
	if err != nil {
		return fmt.Errorf("advisor.loadPhase2: action mix: %w", err)
	}
	for rows.Next() {
		var sid string
		var total, reads, edits, eff int
		if err := rows.Scan(&sid, &total, &reads, &edits, &eff); err != nil {
			rows.Close()
			return fmt.Errorf("advisor.loadPhase2: mix scan: %w", err)
		}
		if s := idx[sid]; s != nil {
			s.Mix = ActionMix{Total: total, Reads: reads, Edits: edits, EffortHigh: eff}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Cachetrack verdicts (may be empty until Anthropic traffic proxies).
	f.CacheEvents = map[string][]CacheEventFact{}
	ceQ := `SELECT session_id, kind, COALESCE(cause,''), COALESCE(tokens_read,0),
	       COALESCE(tokens_written,0), COALESCE(cost_delta_usd,0)
	FROM cache_events WHERE timestamp >= ?`
	rows, err = db.QueryContext(ctx, ceQ, since)
	if err != nil {
		return fmt.Errorf("advisor.loadPhase2: cache_events: %w", err)
	}
	for rows.Next() {
		var sid string
		var e CacheEventFact
		if err := rows.Scan(&sid, &e.Kind, &e.Cause, &e.TokensRead, &e.TokensWritten, &e.CostDeltaUSD); err != nil {
			rows.Close()
			return fmt.Errorf("advisor.loadPhase2: cache scan: %w", err)
		}
		f.CacheEvents[sid] = append(f.CacheEvents[sid], e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// MCP audit counts.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN response_ok = 0 THEN 1 ELSE 0 END), 0) FROM mcp_audit WHERE ts >= ?`,
		since).Scan(&f.MCPCalls, &f.MCPDenied); err != nil {
		return fmt.Errorf("advisor.loadPhase2: mcp_audit: %w", err)
	}
	// "MCP configured" has no single config flag (registration is
	// per-AI-client via observer init) — infer it from lifetime audit
	// rows: any call ever means the server is wired somewhere. Callers
	// may still force it via Options.MCPConfigured.
	if !f.MCPConfigured {
		var lifetime int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_audit`).Scan(&lifetime); err == nil && lifetime > 0 {
			f.MCPConfigured = true
		}
	}

	// Guard posture counts (X3.1): the high-severity verdict load in
	// the window plus the operator's engagement proxy (active scoped
	// approvals). Cheap COUNTs; the mode itself is injected by callers.
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM guard_events WHERE ts >= ? AND severity IN ('high', 'critical')`,
		since).Scan(&f.GuardHighSevEvents); err != nil {
		return fmt.Errorf("advisor.loadPhase2: guard_events: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM guard_approvals WHERE expires_at = '' OR expires_at >= ?`,
		f.Now.UTC().Format(time.RFC3339)).Scan(&f.GuardActiveApprovals); err != nil {
		return fmt.Errorf("advisor.loadPhase2: guard_approvals: %w", err)
	}

	// Failure groups.
	fQ := `SELECT COALESCE(p.root_path,''), fc.command_summary, COUNT(*),
	       SUM(COALESCE(fc.retry_count,0)), MAX(fc.eventually_succeeded)
	FROM failure_context fc LEFT JOIN projects p ON p.id = fc.project_id
	WHERE fc.timestamp >= ? GROUP BY p.root_path, fc.command_summary`
	rows, err = db.QueryContext(ctx, fQ, since)
	if err != nil {
		return fmt.Errorf("advisor.loadPhase2: failures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ff FailureFact
		var rec int
		if err := rows.Scan(&ff.Project, &ff.Command, &ff.Fails, &ff.Retries, &rec); err != nil {
			return fmt.Errorf("advisor.loadPhase2: failure scan: %w", err)
		}
		ff.Recovered = rec != 0
		f.Failures = append(f.Failures, ff)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Scoring-engine metrics + per-session web-search totals (Phase 4).
	sQ := `SELECT s.id, COALESCE(p.root_path,''), s.started_at,
	       s.quality_score, s.stale_reads_wasteful,
	       COALESCE((SELECT SUM(COALESCE(tu.web_search_requests,0)) FROM token_usage tu WHERE tu.session_id = s.id), 0)
	FROM sessions s LEFT JOIN projects p ON p.id = s.project_id
	WHERE s.started_at >= ? ORDER BY s.started_at`
	rows, err = db.QueryContext(ctx, sQ, since)
	if err != nil {
		return fmt.Errorf("advisor.loadPhase2: scores: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sc SessionScores
		var q sql.NullFloat64
		var w sql.NullInt64
		if err := rows.Scan(&sc.SessionID, &sc.Project, &sc.StartedAt, &q, &w, &sc.WebSearchRequests); err != nil {
			return fmt.Errorf("advisor.loadPhase2: score scan: %w", err)
		}
		if q.Valid {
			v := q.Float64
			sc.QualityScore = &v
		}
		if w.Valid {
			v := w.Int64
			sc.StaleReadsWasteful = &v
		}
		f.Scores = append(f.Scores, sc)
	}
	return rows.Err()
}
