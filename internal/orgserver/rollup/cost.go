package rollup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultWindowDays is the trailing window applied when a query asks for a
	// zero/negative number of days.
	DefaultWindowDays = 30
	// maxWindowDays caps the window (matches the OpenAPI WindowDays maximum).
	maxWindowDays = 365
	// topN bounds the top-teams / top-projects / top-models lists.
	topN = 8
)

// days returns the window length normalised to [1, maxWindowDays].
func (w Window) days() int {
	d := w.Days
	if d <= 0 {
		d = DefaultWindowDays
	}
	if d > maxWindowDays {
		d = maxWindowDays
	}
	return d
}

// since returns the RFC3339 lower bound for the window relative to now. The
// pushed-row timestamps are RFC3339 UTC, so a lexicographic `timestamp >= ?`
// comparison is chronological — the same predicate the local dashboard uses.
func since(w Window, now time.Time) string {
	return now.UTC().AddDate(0, 0, -w.days()).Format(time.RFC3339)
}

// ProjectID is the stable, URL-safe identifier for a project_root: the first
// 16 hex chars of its SHA-256. The dashboard uses it as the {id} path param;
// projectRootByID reverses it by scanning the distinct roots present in the
// data (a project_root is not recoverable from the hash alone).
func ProjectID(projectRoot string) string {
	sum := sha256.Sum256([]byte(projectRoot))
	return hex.EncodeToString(sum[:8])
}

// spendCTE is the proxy-deduplicated UNION of the two pre-computed cost columns
// that every spend rollup builds on. It defines a CTE `spend(user_id,
// project_root, model, tokens, ts, cost)`:
//
//   - api_turns contributes cost_usd (proxy-observed, authoritative);
//   - token_usage contributes estimated_cost_usd, EXCEPT rows whose
//     source_event_id is already an api_turns.request_id in the window (those
//     are the same turn seen twice — the proxy row wins).
//
// This mirrors internal/intelligence/dashboard so org spend agrees with each
// developer's local dashboard (spec §1.6, ±2%). It binds three `since` args in
// order: proxy_ids, api_turns, token_usage.
const spendCTE = `
WITH proxy_ids AS (
    SELECT request_id FROM api_turns
     WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ?
),
spend AS (
    SELECT user_id,
           COALESCE(project_root,'') AS project_root,
           COALESCE(model,'')        AS model,
           (COALESCE(input_tokens,0)+COALESCE(output_tokens,0)) AS tokens,
           timestamp                 AS ts,
           COALESCE(cost_usd,0)       AS cost
      FROM api_turns
     WHERE timestamp >= ?
    UNION ALL
    SELECT user_id,
           COALESCE(project_root,'') AS project_root,
           COALESCE(model,'')        AS model,
           (COALESCE(input_tokens,0)+COALESCE(output_tokens,0)) AS tokens,
           timestamp                 AS ts,
           COALESCE(estimated_cost_usd,0) AS cost
      FROM token_usage
     WHERE timestamp >= ?
       AND (source_event_id IS NULL OR source_event_id = ''
            OR source_event_id NOT IN (SELECT request_id FROM proxy_ids))
)`

// spendArgs returns the three `since` bindings spendCTE expects, in order.
func spendArgs(s string) []any { return []any{s, s, s} }

// userScopeSQL returns a boolean SQL fragment restricting <col> (a user_id
// column) to the users in scope, plus its args. An admin scope is always-true;
// a lead scope with no teams is always-false; otherwise it is membership in the
// led teams. The fragment is meant to be AND-ed into a WHERE clause.
func userScopeSQL(col string, scope Scope) (string, []any) {
	if scope.Admin {
		return "1=1", nil
	}
	if len(scope.TeamIDs) == 0 {
		return "1=0", nil
	}
	args := make([]any, len(scope.TeamIDs))
	for i, t := range scope.TeamIDs {
		args[i] = t
	}
	return fmt.Sprintf(
		"%s IN (SELECT user_id FROM org_team_members WHERE team_id IN (%s))",
		col, placeholders(len(scope.TeamIDs)),
	), args
}

// teamScopeSQL returns a boolean fragment restricting a team_id column to the
// teams in scope, plus its args (admin → all teams).
func teamScopeSQL(col string, scope Scope) (string, []any) {
	if scope.Admin {
		return "1=1", nil
	}
	if len(scope.TeamIDs) == 0 {
		return "1=0", nil
	}
	args := make([]any, len(scope.TeamIDs))
	for i, t := range scope.TeamIDs {
		args[i] = t
	}
	return fmt.Sprintf("%s IN (%s)", col, placeholders(len(scope.TeamIDs))), args
}

// placeholders returns "?, ?, ..." with n placeholders (n >= 1).
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Scope restricts a rollup to the whole org (an admin) or a set of teams (a
// team lead). The handler resolves it from the caller's role and passes it in;
// the rollup query trusts it — the defence-in-depth check that a lead cannot
// reach another team's detail/developers URL lives in the handler.
type Scope struct {
	Admin   bool
	TeamIDs []string
}
