package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// SessionFilters are the optional list filters (additive AND). Empty values are
// no-ops. Tool/model are content-free enums (the same dimensions the Tools and
// Models rollups expose).
type SessionFilters struct {
	Tool  string
	Model string
}

// Sessions returns a paginated, scoped, content-free session list for the
// window — the substrate for the org Sessions surface (AUDITED at the handler:
// each row names a developer, the same disclosure class as People). Scope:
// admin → all; lead → the union of their teams' members; plain member → self.
//
// Cost / tokens / api-turn count are computed from the proxy-deduplicated
// api_turns ∪ token_usage substrate keyed by session_id (sessionSpendCTE),
// degrading to token_usage when the proxy did not capture. Project identity is
// the content-free hash-derived ProjectID — the raw project_root path is never
// selected. selfUserID gives a plain member visibility of their own sessions.
func Sessions(ctx context.Context, db *sql.DB, w Window, scope Scope, selfUserID string, f SessionFilters, limit, offset int, now time.Time) (SessionsResult, error) {
	s := since(w, now)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	res := SessionsResult{WindowDays: w.days(), Limit: limit, Offset: offset, Sessions: []SessionRow{}}

	uScope, uArgs := peopleScopeSQL("user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil // nothing in scope
	}

	// Optional content-free filters.
	filterSQL := ""
	filterArgs := []any{}
	if f.Tool != "" {
		filterSQL += " AND tool = ?"
		filterArgs = append(filterArgs, f.Tool)
	}
	if f.Model != "" {
		filterSQL += " AND model = ?"
		filterArgs = append(filterArgs, f.Model)
	}

	// Total count (for the pager) over the scoped+windowed+filtered set.
	//nolint:gosec // G201: uScope/filterSQL are parameterized fragments; all values bind via ?.
	countQ := `SELECT COUNT(*) FROM sessions WHERE COALESCE(started_at,'') >= ? AND ` + uScope + filterSQL
	countArgs := append(append([]any{s}, uArgs...), filterArgs...)
	if err := db.QueryRowContext(ctx, countQ, countArgs...).Scan(&res.Total); err != nil {
		return SessionsResult{}, fmt.Errorf("rollup.Sessions: count: %w", err)
	}
	if res.Total == 0 {
		return res, nil
	}

	// The page itself, newest first.
	//nolint:gosec // G201: uScope/filterSQL are parameterized fragments; all values bind via ?.
	listQ := `
SELECT id, user_id, COALESCE(user_email,''), COALESCE(tool,''), COALESCE(model,''),
       COALESCE(project_root_hash,''), COALESCE(started_at,''), COALESCE(ended_at,''),
       COALESCE(total_actions,0)
  FROM sessions
 WHERE COALESCE(started_at,'') >= ? AND ` + uScope + filterSQL + `
 ORDER BY started_at DESC, id
 LIMIT ? OFFSET ?`
	listArgs := append(append(append([]any{s}, uArgs...), filterArgs...), limit, offset)

	byID := map[string]*SessionRow{}
	order := []string{}
	if err := eachRow(ctx, db, listQ, listArgs, func(rows *sql.Rows) error {
		var r SessionRow
		var hash string
		if err := rows.Scan(&r.SessionID, &r.UserID, &r.Email, &r.Tool, &r.Model,
			&hash, &r.StartedAt, &r.EndedAt, &r.ActionCount); err != nil {
			return err
		}
		if hash != "" {
			r.ProjectID = ProjectIDFromHash(hash)
		}
		cp := r
		byID[r.SessionID] = &cp
		order = append(order, r.SessionID)
		return nil
	}); err != nil {
		return SessionsResult{}, fmt.Errorf("rollup.Sessions: list: %w", err)
	}

	// Enrich the page with deduped cost / tokens / api-turn count per session.
	if err := sessionSpend(ctx, db, s, uScope, uArgs, byID); err != nil {
		return SessionsResult{}, fmt.Errorf("rollup.Sessions: spend: %w", err)
	}
	// Resolve identity (email + display name) from the authoritative SCIM
	// member store (org_members), keeping the session's own user_email as a
	// fallback for a user not in the roster.
	if err := resolveSessionIdentities(ctx, db, byID); err != nil {
		return SessionsResult{}, fmt.Errorf("rollup.Sessions: identity: %w", err)
	}

	out := make([]SessionRow, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	res.Sessions = out
	return res, nil
}

// sessionSpendCTE is the per-session-dimension analogue of spendCTE: the
// proxy-deduplicated UNION of api_turns ∪ token_usage projected with the owning
// session_id so cost / tokens / api-turn count can be summed per session. It is
// deliberately separate from spendCTE (which omits session_id) so that shared
// constant keeps a single owner. Binds spendArgs(s).
const sessionSpendCTE = `
WITH proxy_ids AS (
    SELECT request_id FROM api_turns
     WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ?
),
sspend AS (
    SELECT t.user_id AS user_id, COALESCE(t.session_id,'') AS sid,
           (COALESCE(t.input_tokens,0)+COALESCE(t.output_tokens,0)) AS tokens,
           COALESCE(t.cost_usd,0) AS cost, 1 AS is_turn, t.timestamp AS ts
      FROM api_turns t WHERE t.timestamp >= ?
    UNION ALL
    SELECT tu.user_id AS user_id, COALESCE(tu.session_id,'') AS sid,
           (COALESCE(tu.input_tokens,0)+COALESCE(tu.output_tokens,0)) AS tokens,
           COALESCE(tu.estimated_cost_usd,0) AS cost, 0 AS is_turn, tu.timestamp AS ts
      FROM token_usage tu WHERE tu.timestamp >= ?
       AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
            OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_ids))
)`

// sessionSpend fills CostUSD / Tokens / APITurnCount for the sessions on the
// page (keyed by session id), scoped.
func sessionSpend(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, byID map[string]*SessionRow) error {
	//nolint:gosec // G202: sessionSpendCTE is a code constant; uScope parameterized; values bind via ?.
	q := sessionSpendCTE + `
SELECT sid, COALESCE(SUM(cost),0), COALESCE(SUM(tokens),0), COALESCE(SUM(is_turn),0)
  FROM sspend WHERE sid != '' AND ` + uScope + `
 GROUP BY sid`
	return eachRow(ctx, db, q, append(spendArgs(s), uArgs...), func(rows *sql.Rows) error {
		var sid string
		var cost float64
		var tokens, turns int64
		if err := rows.Scan(&sid, &cost, &tokens, &turns); err != nil {
			return err
		}
		if r, ok := byID[sid]; ok {
			r.CostUSD, r.Tokens, r.APITurnCount = cost, tokens, turns
		}
		return nil
	})
}

// resolveSessionIdentities fills Email + DisplayName for the page's developers
// from the SCIM member store (org_members), the authoritative identity source.
// A session's own user_email is kept as the fallback when the user is absent
// from the roster.
func resolveSessionIdentities(ctx context.Context, db *sql.DB, byID map[string]*SessionRow) error {
	ids := map[string]bool{}
	for _, r := range byID {
		ids[r.UserID] = true
	}
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for id := range ids {
		args = append(args, id)
	}
	//nolint:gosec // G201: only a ?-placeholder list is interpolated; ids bind via ?.
	q := `SELECT user_id, COALESCE(email,''), COALESCE(display_name,'') FROM org_members WHERE user_id IN (` + placeholders(len(args)) + `)`
	type ident struct{ email, disp string }
	got := map[string]ident{}
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var id, email, name string
		if err := rows.Scan(&id, &email, &name); err != nil {
			return err
		}
		got[id] = ident{email, name}
		return nil
	}); err != nil {
		return err
	}
	for _, r := range byID {
		if it, ok := got[r.UserID]; ok {
			if it.email != "" {
				r.Email = it.email
			}
			r.DisplayName = it.disp
		}
	}
	return nil
}

// SessionDetail returns a single session's content-free rollup (token buckets,
// action-type breakdown, cost). found is false when no session with that id is
// in the caller's scope (→ the handler returns 404, which doubles as the
// out-of-scope response so existence is not disclosed). AUDITED at the handler.
func SessionDetail(ctx context.Context, db *sql.DB, id string, scope Scope, selfUserID string, now time.Time) (SessionDetailResult, bool, error) {
	uScope, uArgs := peopleScopeSQL("user_id", scope, selfUserID)
	if uScope == falseScope {
		return SessionDetailResult{}, false, nil
	}

	var res SessionDetailResult
	var hash string
	//nolint:gosec // G201: uScope is a parameterized fragment; id + scope args bind via ?.
	metaQ := `
SELECT id, user_id, COALESCE(user_email,''), COALESCE(tool,''), COALESCE(model,''),
       COALESCE(project_root_hash,''), COALESCE(started_at,''), COALESCE(ended_at,''),
       COALESCE(total_actions,0)
  FROM sessions WHERE id = ? AND ` + uScope + ` LIMIT 1`
	row := db.QueryRowContext(ctx, metaQ, append([]any{id}, uArgs...)...)
	if err := row.Scan(&res.SessionID, &res.UserID, &res.Email, &res.Tool, &res.Model,
		&hash, &res.StartedAt, &res.EndedAt, &res.ActionCount); err != nil {
		if err == sql.ErrNoRows {
			return SessionDetailResult{}, false, nil
		}
		return SessionDetailResult{}, false, fmt.Errorf("rollup.SessionDetail: meta: %w", err)
	}
	if hash != "" {
		res.ProjectID = ProjectIDFromHash(hash)
	}
	res.ActionTypes = []ActionTypeCount{}

	// Identity from the authoritative SCIM member store (session user_email is
	// the fallback when the user is absent from the roster).
	if email, name, err := lookupIdentity(ctx, db, res.UserID); err != nil {
		return SessionDetailResult{}, false, fmt.Errorf("rollup.SessionDetail: identity: %w", err)
	} else {
		if email != "" {
			res.Email = email
		}
		res.DisplayName = name
	}

	// Cost / tokens / api-turn count + token buckets from the deduped substrate.
	if err := sessionDetailSpend(ctx, db, id, &res); err != nil {
		return SessionDetailResult{}, false, fmt.Errorf("rollup.SessionDetail: spend: %w", err)
	}

	// Action-type breakdown (content-free: type enum + counts, never targets).
	if err := sessionActionTypes(ctx, db, id, res.UserID, &res); err != nil {
		return SessionDetailResult{}, false, fmt.Errorf("rollup.SessionDetail: actions: %w", err)
	}

	return res, true, nil
}

// sessionDetailSpend fills CostUSD / Tokens / APITurnCount + the 5 token buckets
// for one session. Cost/tokens come from the deduped api_turns ∪ token_usage
// substrate; buckets come from api_turns (cache read/write are proxy-only),
// degrading to net-input/output from token_usage when the proxy did not capture.
func sessionDetailSpend(ctx context.Context, db *sql.DB, id string, res *SessionDetailResult) error {
	// Deduped cost / tokens / api-turn count for just this session.
	q := sessionSpendCTE + `
SELECT COALESCE(SUM(cost),0), COALESCE(SUM(tokens),0), COALESCE(SUM(is_turn),0)
  FROM sspend WHERE sid = ?`
	args := append(spendArgs(zeroSince()), id)
	if err := db.QueryRowContext(ctx, q, args...).Scan(&res.CostUSD, &res.Tokens, &res.APITurnCount); err != nil {
		return err
	}

	// Proxy token buckets (full 5-bucket where the proxy captured).
	var b TokenBuckets
	pq := `SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_read_tokens),0),
                  COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(output_tokens),0)
             FROM api_turns WHERE session_id = ?`
	if err := db.QueryRowContext(ctx, pq, id).Scan(&b.NetInput, &b.CacheRead, &b.CacheWrite, &b.Output); err != nil {
		return err
	}
	// Reasoning + JSONL fallback for net-input/output when the proxy was absent.
	var ti, to, tr int64
	tq := `SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(reasoning_tokens),0)
             FROM token_usage WHERE session_id = ?`
	if err := db.QueryRowContext(ctx, tq, id).Scan(&ti, &to, &tr); err != nil {
		return err
	}
	b.Reasoning = tr
	if b.NetInput == 0 && b.Output == 0 && b.CacheRead == 0 && b.CacheWrite == 0 {
		// No proxy turns for this session — degrade to JSONL net-input/output.
		b.NetInput, b.Output = ti, to
	}
	res.Buckets = b
	return nil
}

// sessionActionTypes fills the action-type breakdown for one session.
func sessionActionTypes(ctx context.Context, db *sql.DB, id, userID string, res *SessionDetailResult) error {
	q := `SELECT COALESCE(action_type,''), COUNT(*), COALESCE(SUM(success),0)
            FROM actions WHERE session_id = ? AND user_id = ?
           GROUP BY action_type`
	out := []ActionTypeCount{}
	if err := eachRow(ctx, db, q, []any{id, userID}, func(rows *sql.Rows) error {
		var a ActionTypeCount
		if err := rows.Scan(&a.ActionType, &a.Count, &a.SuccessCount); err != nil {
			return err
		}
		if a.ActionType == "" {
			a.ActionType = "unknown"
		}
		out = append(out, a)
		return nil
	}); err != nil {
		return err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ActionType < out[j].ActionType
	})
	res.ActionTypes = out
	return nil
}

// lookupIdentity returns the SCIM email + display name for a user (both empty
// when the user is absent from the roster).
func lookupIdentity(ctx context.Context, db *sql.DB, userID string) (email, name string, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(email,''), COALESCE(display_name,'') FROM org_members WHERE user_id = ?`, userID).Scan(&email, &name)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	return email, name, err
}

// zeroSince returns an all-time lower bound for per-session detail queries (a
// session's turns are a fixed set; the window is a list-only concern).
func zeroSince() string { return "0000-01-01T00:00:00Z" }
