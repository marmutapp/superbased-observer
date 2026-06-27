package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// sparkDays is the length of the per-developer trailing-spend sparkline.
const sparkDays = 7

// People computes the org-wide per-developer leaderboard for the window. This
// is the SAME privacy-sensitive disclosure class as the per-team Developers()
// drill-down — the HANDLER must write the audit_log row before calling this.
//
// Scope: an admin sees every developer; a lead sees the union of their teams'
// members; a plain member (no led teams) sees only themselves (selfUserID).
// Unlike the Teams view it needs no SCIM groups, so it lights up for a
// no-SCIM-groups org where Teams is empty.
//
// Every field is content-free: identity is the SCIM email/display name that
// already ships on the wire; the rest are counts, cost, token volume, and
// tool/model labels.
func People(ctx context.Context, db *sql.DB, w Window, scope Scope, selfUserID string, now time.Time) (PeopleResult, error) {
	s := since(w, now)
	res := PeopleResult{WindowDays: w.days(), People: []PersonRollup{}}
	uScope, uArgs := peopleScopeSQL("user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil // nothing in scope
	}
	if ff, fa := spendFilterSQL(scope); ff != "" {
		uScope += ff
		uArgs = append(uArgs, fa...)
	}

	byID := map[string]*PersonRollup{}
	get := func(id string) *PersonRollup {
		p, ok := byID[id]
		if !ok {
			p = &PersonRollup{UserID: id}
			byID[id] = p
		}
		return p
	}

	// Spend, token volume, last-active per developer (deduped substrate).
	//nolint:gosec // G202: spendCTE is a code constant and uScope is a parameterized scope fragment; values bind via ? args.
	spendQ := spendCTE + `
SELECT user_id, COALESCE(SUM(cost),0), COALESCE(SUM(tokens),0), COALESCE(MAX(ts),'')
  FROM spend WHERE ` + uScope + ` GROUP BY user_id`
	if err := eachRow(ctx, db, spendQ, append(spendArgs(s), uArgs...), func(rows *sql.Rows) error {
		var id, last string
		var cost float64
		var tokens int64
		if err := rows.Scan(&id, &cost, &tokens, &last); err != nil {
			return err
		}
		p := get(id)
		p.CostUSD, p.Tokens, p.LastActive = cost, tokens, last
		return nil
	}); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: spend: %w", err)
	}

	// Session + action counts per developer.
	if err := peopleCount(ctx, db, "sessions", "started_at", s, uScope, uArgs, func(id string, n int64) {
		get(id).SessionCount = n
	}); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: sessions: %w", err)
	}
	if err := peopleCount(ctx, db, "actions", "timestamp", s, uScope, uArgs, func(id string, n int64) {
		get(id).ActionCount = n
	}); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: actions: %w", err)
	}

	// Top tool + top model per developer (argmax over deduped spend).
	if err := peopleTop(ctx, db, "tool", s, uScope, uArgs, func(id, label string) {
		get(id).TopTool = label
	}); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: top tool: %w", err)
	}
	if err := peopleTop(ctx, db, "model", s, uScope, uArgs, func(id, label string) {
		get(id).TopModel = label
	}); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: top model: %w", err)
	}

	// Trailing 7-day spend sparkline per developer.
	if err := peopleSpark(ctx, db, uScope, uArgs, now, byID); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: spark: %w", err)
	}

	if len(byID) == 0 {
		return res, nil
	}

	// Resolve identity (email / display name) from the SCIM member store.
	if err := resolveIdentities(ctx, db, byID); err != nil {
		return PeopleResult{}, fmt.Errorf("rollup.People: identity: %w", err)
	}

	out := make([]PersonRollup, 0, len(byID))
	for _, p := range byID {
		out = append(out, *p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		if out[i].Email != out[j].Email {
			return out[i].Email < out[j].Email
		}
		return out[i].UserID < out[j].UserID
	})
	res.People = out
	return res, nil
}

const falseScope = "1=0"

// peopleScopeSQL restricts a user_id column to the People scope: admin → all;
// lead → the union of led teams; plain member → self only.
func peopleScopeSQL(col string, scope Scope, selfUserID string) (string, []any) {
	if scope.Admin {
		return "1=1", nil
	}
	if len(scope.TeamIDs) > 0 {
		args := make([]any, len(scope.TeamIDs))
		for i, t := range scope.TeamIDs {
			args[i] = t
		}
		return fmt.Sprintf(
			"%s IN (SELECT user_id FROM org_team_members WHERE team_id IN (%s))",
			col, placeholders(len(scope.TeamIDs)),
		), args
	}
	if selfUserID != "" {
		return col + " = ?", []any{selfUserID}
	}
	return falseScope, nil
}

// peopleCount counts rows of <table> in the window per developer.
func peopleCount(ctx context.Context, db *sql.DB, table, tsCol, s, uScope string, uArgs []any, set func(id string, n int64)) error {
	//nolint:gosec // G201: table/tsCol are code-constant identifiers; uScope parameterized; window binds via ?.
	q := fmt.Sprintf(`SELECT user_id, COUNT(*) FROM %s WHERE %s >= ? AND %s GROUP BY user_id`, table, tsCol, uScope)
	return eachRow(ctx, db, q, append([]any{s}, uArgs...), func(rows *sql.Rows) error {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		set(id, n)
		return nil
	})
}

// peopleTop computes the argmax-by-cost label (tool or model) per developer.
func peopleTop(ctx context.Context, db *sql.DB, dim, s, uScope string, uArgs []any, set func(id, label string)) error {
	//nolint:gosec // G202: enrichedCTE is a code constant; dim is one of two code-constant identifiers; uScope parameterized; values bind via ?.
	q := enrichedCTE + `
SELECT user_id, ` + dim + `, COALESCE(SUM(cost),0) c
  FROM ev WHERE ` + dim + ` != '' AND ` + uScope + `
 GROUP BY user_id, ` + dim
	best := map[string]float64{}
	if err := eachRow(ctx, db, q, append(spendArgs(s), uArgs...), func(rows *sql.Rows) error {
		var id, label string
		var c float64
		if err := rows.Scan(&id, &label, &c); err != nil {
			return err
		}
		if c > best[id] {
			best[id] = c
			set(id, label)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// peopleSpark fills the trailing sparkDays-day daily-spend sparkline per
// developer (oldest → newest), zero-filling days with no spend.
func peopleSpark(ctx context.Context, db *sql.DB, uScope string, uArgs []any, now time.Time, byID map[string]*PersonRollup) error {
	s7 := since(Window{Days: sparkDays}, now)
	idx := map[string]int{}
	for i := 0; i < sparkDays; i++ {
		day := now.UTC().AddDate(0, 0, -(sparkDays - 1 - i)).Format("2006-01-02")
		idx[day] = i
	}
	//nolint:gosec // G202: spendCTE is a code constant; uScope parameterized; values bind via ?.
	q := spendCTE + `
SELECT user_id, substr(ts,1,10) AS d, COALESCE(SUM(cost),0)
  FROM spend WHERE ` + uScope + ` GROUP BY user_id, d`
	return eachRow(ctx, db, q, append(spendArgs(s7), uArgs...), func(rows *sql.Rows) error {
		var id, day string
		var cost float64
		if err := rows.Scan(&id, &day, &cost); err != nil {
			return err
		}
		p, ok := byID[id]
		if !ok {
			return nil // a user active in the last 7d but not the main window
		}
		if p.Spark == nil {
			p.Spark = make([]float64, sparkDays)
		}
		if i, ok := idx[day]; ok {
			p.Spark[i] += cost
		}
		return nil
	})
}

// resolveIdentities fills email + display name for the roster from org_members.
func resolveIdentities(ctx context.Context, db *sql.DB, byID map[string]*PersonRollup) error {
	ids := make([]any, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	//nolint:gosec // G201: the only interpolation is a ?-placeholder list; all ids bind via ? args.
	q := fmt.Sprintf(`SELECT user_id, email, COALESCE(display_name,'') FROM org_members WHERE user_id IN (%s)`, placeholders(len(ids)))
	return eachRow(ctx, db, q, ids, func(rows *sql.Rows) error {
		var id, email, disp string
		if err := rows.Scan(&id, &email, &disp); err != nil {
			return err
		}
		if p, ok := byID[id]; ok {
			p.Email, p.DisplayName = email, disp
		}
		return nil
	})
}

// eachRow runs a query and invokes fn for each row, handling close/err.
func eachRow(ctx context.Context, db *sql.DB, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
