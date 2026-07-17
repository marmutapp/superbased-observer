package retention

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// defaultPruneBatch bounds how many rows a single DELETE removes. Aged-row
// pruning walks in batches (rowid-subquery LIMIT loops) so a large backlog
// never takes one giant table lock: each batch is its own implicit transaction,
// letting readers interleave and WAL checkpoints run between batches.
const defaultPruneBatch = 5000

// tsKind selects how a table's retention column is compared to the horizon
// cutoff. It is a capability of the COLUMN (its stored format), not of the
// table's origin — the prune logic branches on this, never on which vendor /
// adapter pushed the row.
type tsKind int

const (
	// kindDatetime: the column stores an RFC3339 instant (event time such as
	// started_at / timestamp / fired_at, or a pushed_at fallback). Compared
	// against an RFC3339 cutoff.
	kindDatetime tsKind = iota
	// kindDate: the column stores a UTC calendar day ("YYYY-MM-DD" — the grain
	// of every daily-aggregate rollup). Compared against a date-only cutoff, so
	// a row whose day equals the horizon boundary is KEPT (exactly N days old),
	// and only strictly-older days are pruned.
	kindDate
)

// retentionTable is one row of the ordered data table that drives aged-row
// pruning (CLAUDE.md #5: an ordered rule set walked top-down, not an if-ladder).
//
//   - name     — the table to prune.
//   - tsColumn — the primary age column.
//   - fallback — an always-present column (typically pushed_at / pulled_at) used
//     via COALESCE when tsColumn may be NULL/empty; "" when tsColumn itself is
//     NOT NULL and never blank (every daily-aggregate `day`).
//   - kind     — how tsColumn / the cutoff are formatted (see tsKind).
type retentionTable struct {
	name     string
	tsColumn string
	fallback string
	kind     tsKind
}

// retentionTables is the ordered set of org-server tables that server-side
// `data_retention_days` prunes when it is positive. It DELETES whole rows past
// the horizon (distinct from the content-body NULLing driven by
// [dashboard.content_retention].otel_content_days — see the package doc and
// PruneAgedRows for how the two interact).
//
// Deliberate table-by-table decisions — what is pruned and by which column, and
// (below) what is EXCLUDED and why. Conservative posture: any table whose role
// is identity, admin-authored config, or a tamper-evident/append-only audit
// record is left intact; when a table's disposition was ambiguous it was
// skipped rather than pruned.
//
// PRUNED — agent-pushed event rows (age by their event time, falling back to
// the always-present pushed_at so a row with a blank event time still ages out):
//   - sessions        (started_at → pushed_at)
//   - actions         (timestamp  → pushed_at)
//   - api_turns       (timestamp  → pushed_at)
//   - token_usage     (timestamp  → pushed_at)
//   - obs_traces      (started_at → pushed_at)  T2 trace structure
//   - obs_spans       (started_at → pushed_at)  T2 span structure
//   - obs_span_events (time       → pushed_at)  T2 span events
//
// PRUNED — agent-pushed daily-aggregate rollups (age by their UTC `day`, which
// is NOT NULL in every one, so no fallback is needed):
//   - routing_summaries    (day)  §R19.4 routing rollup
//   - obs_summaries        (day)  T1 analytics floor
//   - obs_eval_summaries   (day)  T4 eval-run health
//   - obs_enduser_spend    (day)  T5 per-end-user spend
//
// PRUNED — server-polled native-console analytics dailies (day-keyed telemetry
// time-series; not agent-pushed, but org data with a clean age column and no
// identity/config/audit role):
//   - cc_analytics_daily            (day)
//   - codex_analytics_daily         (day)
//   - copilot_analytics_daily       (day)
//   - m365_copilot_analytics_daily  (day)
//
// PRUNED — content-body row stores (whole-row deletion at the data-retention
// horizon; the SEPARATE otel_content_days sweep only NULLs bodies — see the
// precedence note on PruneAgedRows):
//   - otel_content         (timestamp  → pushed_at)  native-OTel bodies
//   - obs_content          (timestamp  → pushed_at)  T3 span-content bodies
//   - m365_copilot_content (created_at → pulled_at)  Rail A content bodies
//
// EXCLUDED — never auto-pruned (documented so a re-audit doesn't "clean them
// up"):
//   - org, org_members, org_teams, org_team_members, org_project_team —
//     IDENTITY / project-mapping. Deleting them is a membership change, not
//     retention.
//   - enrolment_tokens, issued_bearers, revoked_bearers — AUTH/identity.
//     revoked_bearers especially must persist: dropping a revocation while a
//     long-lived bearer is still within its expiry would silently un-revoke it
//     (a security regression). issued_bearers/enrolment_tokens are the
//     identity-adjacent index and are kept with them.
//   - budgets, obs_alert_rules — ADMIN-AUTHORED config (spend caps / alert
//     RULES). Config, not data. (obs_alert_EVENTS, by contrast, ARE pruned.)
//   - org_policy_bundles, org_routing_policies, routing_policy_keys —
//     admin-authored, signed, versioned policy config + signing key; agents
//     depend on the monotonic version history and TOFU-pinned key.
//   - audit_log, routing_policy_audit — append-only AUDIT records.
//   - guard_events — a TAMPER-EVIDENT hash chain (chain_prev/chain_hash);
//     deleting aged links would break the §14.3 chain-continuity probe. Kept on
//     the same audit posture as audit_log even though it carries a timestamp.
//
// obs_alert_events is the one clearly-prunable member of an otherwise-excluded
// migration: alert RULES are config (kept), but the fired-event LOG is
// operational history and ages out like any event row.
var retentionTables = []retentionTable{
	// Core agent-pushed event rows.
	{name: "sessions", tsColumn: "started_at", fallback: "pushed_at", kind: kindDatetime},
	{name: "actions", tsColumn: "timestamp", fallback: "pushed_at", kind: kindDatetime},
	{name: "api_turns", tsColumn: "timestamp", fallback: "pushed_at", kind: kindDatetime},
	{name: "token_usage", tsColumn: "timestamp", fallback: "pushed_at", kind: kindDatetime},

	// Obs T2 trace/span structure (agent-pushed event rows).
	{name: "obs_traces", tsColumn: "started_at", fallback: "pushed_at", kind: kindDatetime},
	{name: "obs_spans", tsColumn: "started_at", fallback: "pushed_at", kind: kindDatetime},
	{name: "obs_span_events", tsColumn: "time", fallback: "pushed_at", kind: kindDatetime},

	// Agent-pushed daily-aggregate rollups (day is NOT NULL — no fallback).
	{name: "routing_summaries", tsColumn: "day", kind: kindDate},
	{name: "obs_summaries", tsColumn: "day", kind: kindDate},
	{name: "obs_eval_summaries", tsColumn: "day", kind: kindDate},
	{name: "obs_enduser_spend", tsColumn: "day", kind: kindDate},

	// Server-polled native-console analytics dailies.
	{name: "cc_analytics_daily", tsColumn: "day", kind: kindDate},
	{name: "codex_analytics_daily", tsColumn: "day", kind: kindDate},
	{name: "copilot_analytics_daily", tsColumn: "day", kind: kindDate},
	{name: "m365_copilot_analytics_daily", tsColumn: "day", kind: kindDate},

	// Operational event log (alert RULES excluded above; EVENTS prune).
	{name: "obs_alert_events", tsColumn: "fired_at", kind: kindDatetime},

	// Content-body row stores (whole-row deletion at this horizon).
	{name: "otel_content", tsColumn: "timestamp", fallback: "pushed_at", kind: kindDatetime},
	{name: "obs_content", tsColumn: "timestamp", fallback: "pushed_at", kind: kindDatetime},
	{name: "m365_copilot_content", tsColumn: "created_at", fallback: "pulled_at", kind: kindDatetime},
}

// TablePruneResult reports how many rows PruneAgedRows deleted from one table.
type TablePruneResult struct {
	Table   string
	Deleted int64
}

// PruneAgedRows DELETES rows older than horizonDays from every table in
// retentionTables, in batches, and returns a per-table deleted count (only
// tables that lost at least one row are reported). A horizonDays ≤ 0 is a no-op
// (retention disabled — keep forever), matching PruneOTelContent's semantics.
//
// This is INDEPENDENT of the content-body NULLing (PruneOTelContent /
// PruneObsContent, driven by otel_content_days). Precedence when both apply to
// the same content table (otel_content, obs_content): the body-NULL sweep
// clears bodies at its (typically shorter) horizon while keeping the row +
// content_hash; PruneAgedRows removes the whole row at the data-retention
// horizon. Whole-row deletion simply supersedes a NULLed body — the two never
// conflict, regardless of which horizon is larger.
//
// Batching keeps a huge backlog from locking the DB: each table is drained
// defaultPruneBatch rows at a time until a batch removes nothing, checking ctx
// between batches so a shutdown stops the sweep promptly.
func PruneAgedRows(ctx context.Context, db *sql.DB, horizonDays int, now time.Time) ([]TablePruneResult, error) {
	if horizonDays <= 0 {
		return nil, nil
	}
	base := now.UTC().AddDate(0, 0, -horizonDays)
	dtCutoff := base.Format(time.RFC3339)
	dateCutoff := base.Format("2006-01-02")

	var results []TablePruneResult
	for _, t := range retentionTables {
		cutoff := dtCutoff
		if t.kind == kindDate {
			cutoff = dateCutoff
		}
		n, err := pruneTable(ctx, db, t, cutoff, defaultPruneBatch)
		if err != nil {
			return results, fmt.Errorf("retention.PruneAgedRows: %s: %w", t.name, err)
		}
		if n > 0 {
			results = append(results, TablePruneResult{Table: t.name, Deleted: n})
		}
	}
	return results, nil
}

// pruneTable drains one table in rowid-subquery batches of at most batch rows
// until a batch deletes nothing (or ctx is cancelled), returning the total
// deleted. The rowid-IN-(SELECT … LIMIT) form is used rather than
// `DELETE … LIMIT` because the latter needs a non-default SQLite compile flag
// (SQLITE_ENABLE_UPDATE_DELETE_LIMIT) that modernc.org/sqlite does not set.
func pruneTable(ctx context.Context, db *sql.DB, t retentionTable, cutoff string, batch int) (int64, error) {
	// Age expression: COALESCE onto the always-present fallback when tsColumn
	// may be blank; otherwise compare tsColumn directly.
	ageExpr := t.tsColumn
	if t.fallback != "" {
		ageExpr = fmt.Sprintf("COALESCE(NULLIF(%s,''), %s)", t.tsColumn, t.fallback)
	}
	// Identifiers are in-package literals from retentionTables; no user input
	// reaches this string (gosec G201 would be a false positive).
	//nolint:gosec // G201: table/column identifiers are in-package literals from retentionTables; the cutoff value is bound via a placeholder.
	query := fmt.Sprintf(
		"DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s < ? LIMIT %d)",
		t.name, t.name, ageExpr, batch,
	)

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := db.ExecContext(ctx, query, cutoff)
		if err != nil {
			return total, fmt.Errorf("delete batch: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("rows: %w", err)
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}
