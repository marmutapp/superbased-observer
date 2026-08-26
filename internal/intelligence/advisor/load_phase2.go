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

	// Action mix + effort. actions has no index covering
	// (session_id, timestamp), so a SQL-side `GROUP BY session_id` over
	// the timestamp-filtered window would force the same kind of temp
	// B-tree spill as an ORDER BY on this table (P1-C). Instead: fetch
	// the per-row flags (still computed in SQL — json_extract on the
	// nullable metadata blob is NULL-safe, returns NULL → not counted)
	// unaggregated, and sum them per session in Go.
	mixQ := `SELECT session_id,
	       CASE WHEN action_type IN ('read_file','search_text','search_files') THEN 1 ELSE 0 END,
	       CASE WHEN action_type IN ('edit_file','write_file') THEN 1 ELSE 0 END,
	       CASE WHEN json_extract(metadata,'$.effort_level') IN ('xhigh','max') THEN 1 ELSE 0 END
	FROM actions WHERE timestamp >= ?`
	rows, err := db.QueryContext(ctx, mixQ, since)
	if err != nil {
		return fmt.Errorf("advisor.loadPhase2: action mix: %w", err)
	}
	mix := map[string]*ActionMix{}
	for rows.Next() {
		var sid string
		var isRead, isEdit, isEff int
		if err := rows.Scan(&sid, &isRead, &isEdit, &isEff); err != nil {
			rows.Close()
			return fmt.Errorf("advisor.loadPhase2: mix scan: %w", err)
		}
		m, ok := mix[sid]
		if !ok {
			m = &ActionMix{}
			mix[sid] = m
		}
		m.Total++
		m.Reads += isRead
		m.Edits += isEdit
		m.EffortHigh += isEff
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for sid, m := range mix {
		if s := idx[sid]; s != nil {
			s.Mix = *m
		}
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

	// Handoff targets (node-local handoffs table, read-only).
	if err := loadHandoffTargets(ctx, db, idx); err != nil {
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

// loadHandoffTargets marks sessions that are handoff targets (node-local
// handoffs table, read-only). A session stamped as a handoff target opens
// with a large cold rehydration write BY DESIGN — the balloon +
// cache-write-waste detectors exempt that leading row so the by-design
// write never reads as waste. This is the retroactive belt for the live
// cachetrack handoff_rehydration cause: it also covers non-proxied targets
// whose live flag never fired. Best-effort by shape: a missing/legacy
// handoffs table (pre-migration-055) is not an error. The table is one row
// per handoff (small), so no window filter is needed.
func loadHandoffTargets(ctx context.Context, db *sql.DB, idx map[string]*SessionFacts) error {
	hrows, herr := db.QueryContext(ctx,
		`SELECT target_session_id FROM handoffs WHERE target_session_id IS NOT NULL AND target_session_id != ''`)
	if herr != nil {
		// Missing/legacy table — not an error (best-effort by shape).
		return nil
	}
	defer hrows.Close()
	for hrows.Next() {
		var tsid string
		if err := hrows.Scan(&tsid); err != nil {
			return fmt.Errorf("advisor.loadPhase2: handoff target scan: %w", err)
		}
		if s := idx[tsid]; s != nil {
			s.HandoffTarget = true
		}
	}
	return hrows.Err()
}
