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
// 16 hex chars of its SHA-256. The dashboard uses it as the {id} path param.
// Two callers, two paths:
//
//   - ProjectID(rawRoot): used when the raw path is available (full-content
//     mode); computes sha256 explicitly.
//   - ProjectIDFromHash(hash): used when only the project_root_hash is on
//     hand (metadata-only mode); the hash already IS sha256(rawRoot), so a
//     prefix slice suffices — re-hashing would invert the privacy property.
//
// Both functions yield the same 16-char id for the same underlying project.
func ProjectID(projectRoot string) string {
	sum := sha256.Sum256([]byte(projectRoot))
	return hex.EncodeToString(sum[:8])
}

// ProjectIDFromHash returns the 16-char ProjectID for a precomputed
// project_root_hash (the full sha256 hex of the raw path). When the hash
// is shorter than 16 chars (malformed / empty), the whole hash is
// returned — the {id} URL still round-trips through projectRootByID,
// which scans by hash.
func ProjectIDFromHash(hash string) string {
	if len(hash) >= 16 {
		return hash[:16]
	}
	return hash
}

// spendCTE is the proxy-deduplicated UNION of the two pre-computed cost columns
// that every spend rollup builds on. It defines a CTE `spend(user_id,
// project_root_hash, model, tokens, ts, cost, tool)`:
//
//   - api_turns contributes cost_usd (proxy-observed, authoritative);
//   - token_usage contributes estimated_cost_usd, EXCEPT rows whose
//     source_event_id is already an api_turns.request_id in the window (those
//     are the same turn seen twice — the proxy row wins).
//
// The project dimension is keyed by `project_root_hash`, not the raw
// `project_root` column: v1.8.0's privacy posture strips the raw column to ”
// by default (only the hash ships server-side). Each branch LEFT JOINs
// `sessions` so a proxy-fed `api_turns` row whose own `project_root_hash` is
// empty (its `project_id` was NULL on the agent → the agent-side LEFT JOIN
// `projects` yielded no hash) still inherits the session's hash via
// `session_id`. Without that fallback, every project-dimension rollup
// returns empty under the default privacy posture (D1 in
// docs/teams-test-open-issues-2026-06-04.md).
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
    SELECT t.user_id,
           COALESCE(NULLIF(t.project_root_hash,''), NULLIF(s.project_root_hash,''), '') AS project_root_hash,
           COALESCE(t.model,'')        AS model,
           (COALESCE(t.input_tokens,0)+COALESCE(t.output_tokens,0)) AS tokens,
           t.timestamp                 AS ts,
           COALESCE(t.cost_usd,0)      AS cost,
           COALESCE(s.tool,'')         AS tool
      FROM api_turns t
      LEFT JOIN sessions s ON s.id = t.session_id AND s.user_id = t.user_id
     WHERE t.timestamp >= ?
    UNION ALL
    SELECT tu.user_id,
           COALESCE(NULLIF(tu.project_root_hash,''), NULLIF(s.project_root_hash,''), '') AS project_root_hash,
           COALESCE(tu.model,'')        AS model,
           (COALESCE(tu.input_tokens,0)+COALESCE(tu.output_tokens,0)) AS tokens,
           tu.timestamp                 AS ts,
           COALESCE(tu.estimated_cost_usd,0) AS cost,
           COALESCE(NULLIF(tu.tool,''), NULLIF(s.tool,''), '') AS tool
      FROM token_usage tu
      LEFT JOIN sessions s ON s.id = tu.session_id AND s.user_id = tu.user_id
     WHERE tu.timestamp >= ?
       AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
            OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_ids))
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
//
// Filters carries the OPTIONAL global presentation filter (§6c-3) that the
// handler reads from a query param. It is NOT authorization (the authz
// dimensions are Admin/TeamIDs above); it rides on Scope purely so every
// already-Scope-threaded rollup + the Cached key picks it up without a
// signature change. An empty Filters appends no SQL, so a rollup's query and
// golden output are byte-identical to the unfiltered path.
//
// The filter is TOOL-only by design: `tool` is the one content-free dimension
// present uniformly on every substrate the global rollups touch — the spend
// CTE (resolved via the session join for api_turns), the enriched ev CTE,
// sessions, and actions. The project dimension lives on the `actions` table as
// a session-derived join only (no project_root_hash column there), and adding
// one would be a forbidden agent wire-column change — so per-project drill-down
// stays on the dedicated Projects page rather than the global filter.
type Scope struct {
	Admin   bool
	TeamIDs []string
	Filters Filters
}

// Filters is the optional content-free global filter: a tool name (e.g.
// "claude-code"). `tool` is a column the spend CTE projects and that sessions /
// actions / token_usage expose directly.
type Filters struct {
	Tool string
}

// spendFilterSQL returns an AND-prefixed fragment (and its args) restricting a
// spend / ev / sessions / actions row set to Scope.Filters, referencing the
// `tool` column those substrates all expose. An empty filter → empty fragment,
// so the caller's query is unchanged.
func spendFilterSQL(scope Scope) (string, []any) {
	if scope.Filters.Tool == "" {
		return "", nil
	}
	return " AND tool = ?", []any{scope.Filters.Tool}
}
