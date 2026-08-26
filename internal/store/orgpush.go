package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// schema_meta keys holding the per-table push cursor (the highest table id /
// sessions.rowid already shipped to the org server). Using insertion-ordered
// ids — not event timestamps — means backfilled rows, which receive fresh
// high ids regardless of their event time, are still captured by a later push.
const (
	pushCursorKeySessions    = "org_push_cursor_sessions"
	pushCursorKeyActions     = "org_push_cursor_actions"
	pushCursorKeyAPITurns    = "org_push_cursor_api_turns"
	pushCursorKeyTokenUsage  = "org_push_cursor_token_usage" //nolint:gosec // G101: schema_meta cursor key name, not a credential.
	pushCursorKeyGuardEvents = "org_push_cursor_guard_events"
	pushCursorKeyOTelContent = "org_push_cursor_otel_content"
	// lastPushPayloadKey holds the JSON of the most recent successfully-pushed
	// envelope (the content-free rollup, exactly as marshalled before gzip), so
	// the dashboard can show the developer precisely what was shared. Overwritten
	// each successful push; cleared on unenrol.
	lastPushPayloadKey = "org_last_push_payload"
	// orgPolicyETagKey holds the ETag of the last verified policy bundle
	// (guard spec §14.2) so the hourly poll sends If-None-Match and an
	// unchanged bundle costs a 304 instead of a re-download + re-verify.
	orgPolicyETagKey = "org_policy_bundle_etag"
)

// Org-tier observability push bounds (obs-org-tier plan §4). T1/T4 recompute a
// recent window; T2/T3 cap the per-push row count (windowed-recompute v1 — the
// server upserts by composite key, so a re-pushed window is idempotent).
const (
	obsOrgWindowDays = 7
	obsOrgRowCap     = 5000
)

// ObsOrgProviders is the obs→org bridge: the host binds these funcs at the obs
// wiring point (cmd/observer/obs_wire.go) over reads that internal/obs OWNS
// (internal/obs/store), returning plain orgcontract rows. internal/store never
// imports internal/obs — the coupling is exactly these four removable funcs
// (rule #4). A nil func means that tier is unavailable (no_obs / obs disabled).
type ObsOrgProviders struct {
	Summaries func(ctx context.Context, windowDays int) ([]orgcontract.ObsSummaryRow, error)                     // T1
	Spans     func(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsSpanBatch, error)    // T2
	Content   func(ctx context.Context, cur orgcontract.ObsCursor, max int) ([]orgcontract.ObsContentRow, error) // T3
	EvalRuns  func(ctx context.Context, windowDays int) ([]orgcontract.ObsEvalRow, error)                        // T4
	// EndUserSpend is the T5 per-end-user spend aggregate (org-budget
	// guardrails plan §2.1). End-user id is PII, so the host composes it
	// ONLY under ObsSummary && shipsRawContent(). nil when obs is compiled
	// out (no_obs) or the provider isn't wired.
	EndUserSpend func(ctx context.Context, windowDays int) ([]orgcontract.ObsEndUserSpendRow, error) // T5 (PII, gated)

	// Admission is the T6 input-admission provider (Plane-A admission org
	// tier, gap-audit 2026-07-10 §2.1 / #1a): the window's verdict events +
	// the policy snapshots they reference. The verdict PII/prose columns
	// (Tenant/EndUser/ReasonExcerpt) ride raw out of the provider; the host
	// strips them under !shipsRawContent() exactly like the other content
	// tiers (policy Body always ships — it is admin-authored config). nil when
	// obs is compiled out (no_obs) or the provider isn't wired.
	Admission func(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsAdmissionBatch, error) // T6

	// EvalItems is the T7 per-item eval provider (Plane-A eval-run detail org
	// tier, gap-audit 2026-07-10 §1 / §2.2 / §6): the window's per-item eval
	// scores joined to their run/dataset/item for identity + content. The item
	// content excerpts (input/expected/output/rationale) ride raw out of the
	// provider; the host strips them under !shipsRawContent() exactly like the
	// other content tiers (the content_hash always ships). nil when obs is
	// compiled out (no_obs) or the provider isn't wired.
	EvalItems func(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsEvalItemBatch, error) // T7
	// Egress is the T8 egress-routing decision provider (W5.3): what the
	// node's compiled routing policy decided for outbound model/provider
	// calls. nil when obs is compiled out or the tier is off.
	Egress func(ctx context.Context, cur orgcontract.ObsCursor, max int) (orgcontract.ObsEgressBatch, error) // T8
}

// PushCursor is the agent's per-table push position. Each field is the
// highest row id (sessions: rowid) already accepted by the server. Rows above
// it are candidates for the next batch.
type PushCursor struct {
	Sessions    int64
	Actions     int64
	APITurns    int64
	TokenUsage  int64
	GuardEvents int64
	OTelContent int64
}

// PushBatch is one batch of content-free rows read from the agent DB, plus the
// cursor the agent should persist if the server accepts it.
type PushBatch struct {
	Cursor      PushCursor
	Sessions    []orgcontract.SessionRow
	Actions     []orgcontract.ActionRow
	APITurns    []orgcontract.APITurnRow
	TokenUsage  []orgcontract.TokenUsageRow
	GuardEvents []orgcontract.GuardEventRow
	OTelContent []orgcontract.OTelContentRow
	// RoutingSummaries is the OPTIONAL §R19.4 aggregate, attached only
	// under ShareOptions.RoutingSummary. It rides along with row data
	// and does not affect cursors (the server upsert is idempotent).
	RoutingSummaries []orgcontract.RoutingSummaryRow
	// CacheSummaries is the OPTIONAL Arc 4 P5c cache-detail aggregate,
	// attached only under ShareOptions.CacheDetail and computed by
	// store.SelectCacheSummaries (which owns the node-local cache_events
	// read; this file names no cache_* table). Windowed-recompute +
	// server-upsert-idempotent, so it does not affect the per-table cursors.
	CacheSummaries []orgcontract.CacheSummaryRow
	// CodeintelSummaries is the OPTIONAL Arc 4 P5f codeintel-detail aggregate,
	// attached only under ShareOptions.CodeintelDetail and computed by
	// store.SelectCodeintelSummaries (which owns the node-local codeintel_*
	// read; this file names no codeintel_* table). Snapshot-recompute +
	// server-upsert-idempotent, so it does not affect the per-table cursors.
	CodeintelSummaries []orgcontract.CodeintelSummaryRow
	// ProcessSummaries is the OPTIONAL Arc 4 P5g process-detail aggregate,
	// attached only under ShareOptions.ProcessDetail and computed by
	// store.SelectProcessSummaries (which owns the node-local process_runs
	// read; this file names no process_* table). Windowed-recompute +
	// server-upsert-idempotent, so it does not affect the per-table cursors.
	ProcessSummaries []orgcontract.ProcessSummaryRow
	// SessionVerbositySummaries / SessionCacheSummaries / SessionProcesses
	// are the org-parity session-scoped enterprise wires (W3.1 / W2.1 /
	// W2.2), attached only under share.shipsRawContent() (FullContent ||
	// AdminManaged — deliberately NOT the teams-tier detail flags above;
	// session-scoped telemetry is per-developer detail the enterprise
	// posture treats as admin-visible-by-default). Computed by
	// store.SelectSessionVerbositySummaries / SelectSessionCacheSummaries /
	// SelectSessionProcessRows, which own the node-local reads — this file
	// names none of those tables (the privacy sentinel forbids it).
	// Windowed-recompute + server-upsert-idempotent; no cursor effect.
	SessionVerbositySummaries []orgcontract.SessionVerbosityRow
	SessionCacheSummaries     []orgcontract.SessionCacheRow
	SessionProcesses          []orgcontract.SessionProcessRow
	SessionNetworkEvents      []orgcontract.SessionNetworkEventRow
	// Wave-3 per-developer enterprise wires — same shipsRawContent() gate
	// and one-owner Select-file discipline as the session wires above.
	AdvisorSuggestions []orgcontract.AdvisorSuggestionRow
	ProjectPatterns    []orgcontract.ProjectPatternRow
	BenchmarkRuns      []orgcontract.BenchmarkRunRow
	BenchmarkAttempts  []orgcontract.BenchmarkAttemptRow
	CompressionStats   []orgcontract.CompressionStatRow
	RoutingDevRows     []orgcontract.RoutingDevRow
	CodeintelDevRows   []orgcontract.CodeintelDevRow
	TerminalRuns       []orgcontract.TerminalRunRow
	TerminalCommands   []orgcontract.TerminalCommandRow
	RemoteAudit        []orgcontract.RemoteAuditRow
	GuardPins          []orgcontract.GuardPinRow
	GuardApprovals     []orgcontract.GuardApprovalRow
	// TerminalSummaries + RemoteAuditSummaries are the OPTIONAL Arc 4 P5h
	// terminal-detail aggregates, attached only under ShareOptions.TerminalDetail
	// and computed by store.SelectTerminalSummaries / SelectRemoteAuditSummaries
	// (which own the node-local terminal_* / remote_audit reads; this file names
	// none of those tables). Windowed-recompute + server-upsert-idempotent, so
	// they do not affect the per-table cursors.
	TerminalSummaries    []orgcontract.TerminalSummaryRow
	RemoteAuditSummaries []orgcontract.RemoteAuditSummaryRow
	// RoutingDetails is the OPTIONAL Arc 4 P5d routing-detail aggregate,
	// attached only under ShareOptions.RoutingDetail and computed by
	// store.SelectRoutingDetail (which owns the node-local router_decisions
	// read; this file names no such table). Windowed-recompute +
	// server-upsert-idempotent, so it does not affect the per-table cursors.
	RoutingDetails []orgcontract.RoutingDetailRow
	// LimitGauges is the OPTIONAL Arc 4 P5e predictions aggregate, attached
	// only under ShareOptions.LimitGauge and computed by store.SelectLimitGauges
	// (which owns the node-local limit_snapshots read; this file names no such
	// table). Windowed-recompute + server-upsert-idempotent.
	LimitGauges []orgcontract.LimitGaugeRow
	// Obs* are the OPTIONAL org-tier observability rollups
	// (obs-org-tier plan), each attached only under its own ShareOptions
	// flag and composed via the obs provider seam (this file names no
	// obs_* table). All windowed-recompute + server-upsert-idempotent, so
	// none affects the per-table cursors.
	ObsSummaries  []orgcontract.ObsSummaryRow   // T1
	ObsTraces     []orgcontract.ObsTraceRow     // T2
	ObsSpans      []orgcontract.ObsSpanRow      // T2
	ObsSpanEvents []orgcontract.ObsSpanEventRow // T2
	ObsContent    []orgcontract.ObsContentRow   // T3
	ObsEvalRuns   []orgcontract.ObsEvalRow      // T4
	// ObsEndUserSpend is the T5 per-end-user spend aggregate (org-budget
	// guardrails plan §2.1), attached only under ObsSummary &&
	// shipsRawContent() (end-user PII). Aggregate — NOT counted by RowCount
	// (recomputes idempotently), but consulted by hasAggregates so an
	// aggregate-only batch still pushes.
	ObsEndUserSpend []orgcontract.ObsEndUserSpendRow // T5 (PII, gated)
	// ObsAdmissionEvents / ObsAdmissionPolicies are the OPTIONAL T6
	// input-admission tier (Plane-A admission org, gap-audit §2.1 / #1a),
	// attached only under ShareOptions.ObsAdmission and composed via the obs
	// provider seam (this file names no obs_* table). Events are counted by
	// RowCount (row-bearing, like the T2/T3 structure rows); Policies are
	// consulted by hasAggregates (windowed-recompute, server-upsert
	// idempotent) so a policies-only batch still pushes. Neither affects the
	// per-table cursors.
	ObsAdmissionEvents   []orgcontract.ObsAdmissionRow       // T6 events
	ObsAdmissionPolicies []orgcontract.ObsAdmissionPolicyRow // T6 policies
	// ObsEvalItems is the OPTIONAL T7 per-item eval tier (Plane-A eval-run
	// detail org, gap-audit §1 / §2.2 / §6), attached only under
	// ShareOptions.ObsEvalItems and composed via the obs provider seam (this
	// file names no obs_* table). Row-bearing (counted by RowCount, like the
	// T2/T3 structure rows + T6 events); windowed-recompute + server-upsert
	// idempotent, so it does not affect the per-table cursors.
	ObsEvalItems []orgcontract.ObsEvalItemRow // T7 per-item eval scores
	// ObsEgressDecisions is the OPTIONAL T8 egress-routing decision feed
	// (W5.3), attached only under ShareOptions.ObsEgress and composed via
	// the obs provider seam (this file names no obs_* table).
	ObsEgressDecisions []orgcontract.ObsEgressRow // T8 egress routing decisions
	EstBytes           int64
}

// RowCount is the total number of rows across all row-bearing tables in the
// batch (the aggregate rollups — routing/obs summaries — are not counted; they
// recompute idempotently and don't gate batch progress).
func (b PushBatch) RowCount() int {
	return len(b.Sessions) + len(b.Actions) + len(b.APITurns) + len(b.TokenUsage) +
		len(b.GuardEvents) + len(b.OTelContent) +
		len(b.ObsTraces) + len(b.ObsSpans) + len(b.ObsSpanEvents) + len(b.ObsContent) +
		len(b.ObsAdmissionEvents) + len(b.ObsEvalItems) + len(b.ObsEgressDecisions)
}

// hasAggregates reports whether the batch carries any windowed-recompute
// aggregate rollups that RowCount deliberately omits (they recompute
// idempotently and don't gate cursor progress): the routing summaries and the
// obs T1/T4 aggregates. A node opted into an aggregate-only tier (e.g.
// obs_summary with no trace rows) has zero RowCount but must still push, so the
// aggregates are consulted here rather than being silently dropped by the
// PushOnce empty-batch early return.
func (b PushBatch) hasAggregates() bool {
	return len(b.RoutingSummaries) > 0 || len(b.CacheSummaries) > 0 ||
		len(b.CodeintelSummaries) > 0 || len(b.ProcessSummaries) > 0 ||
		len(b.SessionVerbositySummaries) > 0 || len(b.SessionCacheSummaries) > 0 ||
		len(b.SessionProcesses) > 0 || len(b.SessionNetworkEvents) > 0 ||
		len(b.AdvisorSuggestions) > 0 || len(b.ProjectPatterns) > 0 ||
		len(b.BenchmarkRuns) > 0 || len(b.BenchmarkAttempts) > 0 ||
		len(b.CompressionStats) > 0 ||
		len(b.RoutingDevRows) > 0 || len(b.CodeintelDevRows) > 0 ||
		len(b.TerminalRuns) > 0 || len(b.TerminalCommands) > 0 ||
		len(b.RemoteAudit) > 0 ||
		len(b.GuardPins) > 0 || len(b.GuardApprovals) > 0 ||
		len(b.TerminalSummaries) > 0 || len(b.RemoteAuditSummaries) > 0 ||
		len(b.RoutingDetails) > 0 || len(b.LimitGauges) > 0 ||
		len(b.ObsSummaries) > 0 ||
		len(b.ObsEvalRuns) > 0 || len(b.ObsEndUserSpend) > 0 ||
		len(b.ObsAdmissionPolicies) > 0
}

// Empty reports whether the batch carries nothing to push — no rows AND no
// aggregate rollups. An aggregate-only batch is NOT empty (see hasAggregates).
func (b PushBatch) Empty() bool { return b.RowCount() == 0 && !b.hasAggregates() }

// PushLogEntry is one row of org_push_log, surfaced to the dashboard.
type PushLogEntry struct {
	ID       int64
	PushedAt string
	RowCount int64
	Bytes    int64
	Status   string
	Error    string
}

// LoadPushCursor reads the per-table push cursor from schema_meta. Missing
// keys (a never-pushed agent) read as 0.
func (s *Store) LoadPushCursor(ctx context.Context) (PushCursor, error) {
	var c PushCursor
	for key, dst := range map[string]*int64{
		pushCursorKeySessions:    &c.Sessions,
		pushCursorKeyActions:     &c.Actions,
		pushCursorKeyAPITurns:    &c.APITurns,
		pushCursorKeyTokenUsage:  &c.TokenUsage,
		pushCursorKeyGuardEvents: &c.GuardEvents,
		pushCursorKeyOTelContent: &c.OTelContent,
	} {
		v, err := s.readMeta(ctx, key)
		if err != nil {
			return PushCursor{}, fmt.Errorf("store.LoadPushCursor: %w", err)
		}
		if v != "" {
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				return PushCursor{}, fmt.Errorf("store.LoadPushCursor: parse %s=%q: %w", key, v, perr)
			}
			*dst = n
		}
	}
	return c, nil
}

// SavePushCursor persists the per-table push cursor to schema_meta in one tx.
func (s *Store) SavePushCursor(ctx context.Context, c PushCursor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.SavePushCursor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for key, val := range map[string]int64{
		pushCursorKeySessions:    c.Sessions,
		pushCursorKeyActions:     c.Actions,
		pushCursorKeyAPITurns:    c.APITurns,
		pushCursorKeyTokenUsage:  c.TokenUsage,
		pushCursorKeyGuardEvents: c.GuardEvents,
		pushCursorKeyOTelContent: c.OTelContent,
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_meta(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, strconv.FormatInt(val, 10)); err != nil {
			return fmt.Errorf("store.SavePushCursor: set %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.SavePushCursor: commit: %w", err)
	}
	return nil
}

// CurrentMaxIDs returns the current high-water id of each table. Enrolment
// seeds the push cursor from this so only activity *after* enrolment is
// shared — the agent never retroactively pushes a developer's pre-enrolment
// history.
func (s *Store) CurrentMaxIDs(ctx context.Context) (PushCursor, error) {
	var c PushCursor
	for q, dst := range map[string]*int64{
		`SELECT COALESCE(MAX(rowid), 0) FROM sessions`:  &c.Sessions,
		`SELECT COALESCE(MAX(id), 0) FROM actions`:      &c.Actions,
		`SELECT COALESCE(MAX(id), 0) FROM api_turns`:    &c.APITurns,
		`SELECT COALESCE(MAX(id), 0) FROM token_usage`:  &c.TokenUsage,
		`SELECT COALESCE(MAX(id), 0) FROM guard_events`: &c.GuardEvents,
		`SELECT COALESCE(MAX(id), 0) FROM otel_content`: &c.OTelContent,
	} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return PushCursor{}, fmt.Errorf("store.CurrentMaxIDs: %w", err)
		}
	}
	return c, nil
}

// RecordPush appends a row to org_push_log. status is 'ok' | 'retry' | 'failed'.
func (s *Store) RecordPush(ctx context.Context, rowCount, byteCount int64, status, errMsg string) error {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_push_log(pushed_at, row_count, byte_count, status, error)
		 VALUES (datetime('now'), ?, ?, ?, ?)`,
		rowCount, byteCount, status, errVal)
	if err != nil {
		return fmt.Errorf("store.RecordPush: %w", err)
	}
	return nil
}

// SaveLastPushPayload records the JSON of the most recent successfully-pushed
// envelope (the content-free rollup) so the dashboard can show what was shared.
func (s *Store) SaveLastPushPayload(ctx context.Context, payload []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		lastPushPayloadKey, string(payload)); err != nil {
		return fmt.Errorf("store.SaveLastPushPayload: %w", err)
	}
	return nil
}

// LoadLastPushPayload returns the JSON of the last pushed envelope, or nil when
// the agent has never pushed (or has unenrolled).
func (s *Store) LoadLastPushPayload(ctx context.Context) ([]byte, error) {
	v, err := s.readMeta(ctx, lastPushPayloadKey)
	if err != nil {
		return nil, fmt.Errorf("store.LoadLastPushPayload: %w", err)
	}
	if v == "" {
		return nil, nil
	}
	return []byte(v), nil
}

// SaveOrgPolicyETag records the ETag of the most recently verified policy
// bundle (guard spec §14.2). Overwritten on every applied fetch.
func (s *Store) SaveOrgPolicyETag(ctx context.Context, etag string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		orgPolicyETagKey, etag); err != nil {
		return fmt.Errorf("store.SaveOrgPolicyETag: %w", err)
	}
	return nil
}

// LoadOrgPolicyETag returns the last verified bundle's ETag, or "" when the
// agent has never applied a bundle.
func (s *Store) LoadOrgPolicyETag(ctx context.Context) (string, error) {
	v, err := s.readMeta(ctx, orgPolicyETagKey)
	if err != nil {
		return "", fmt.Errorf("store.LoadOrgPolicyETag: %w", err)
	}
	return v, nil
}

// LastPushLog returns the most recent org_push_log row, or (nil, nil) when the
// agent has never pushed.
func (s *Store) LastPushLog(ctx context.Context) (*PushLogEntry, error) {
	var e PushLogEntry
	var errVal sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, pushed_at, row_count, byte_count, status, error
		   FROM org_push_log ORDER BY id DESC LIMIT 1`).
		Scan(&e.ID, &e.PushedAt, &e.RowCount, &e.Bytes, &e.Status, &errVal)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store.LastPushLog: %w", err)
	}
	e.Error = errVal.String
	return &e, nil
}

// ClearLastPushState removes the prior-run last-push payload + log so
// `observer org status` after a re-enroll shows "(none yet)" instead of
// a stale timestamp. Called by orgclient.Enroll alongside the cursor
// seed; idempotent on a never-pushed agent. N5 in
// docs/teams-test-regression-2026-06-03.md.
func (s *Store) ClearLastPushState(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.ClearLastPushState: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_meta WHERE key = ?`, lastPushPayloadKey); err != nil {
		return fmt.Errorf("store.ClearLastPushState: clear payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_push_log`); err != nil {
		return fmt.Errorf("store.ClearLastPushState: clear log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.ClearLastPushState: commit: %w", err)
	}
	return nil
}

// ScopeOptions restricts which projects feed the push by exact
// project_root path. Empty Allowlist + empty Denylist (the zero value)
// means "every project ships" — preserving v1.7.x behaviour. When
// Allowlist is non-empty, only listed roots are eligible; Denylist
// then strips anything matching from the result.
//
// Implementation note: SelectUnpushedSince resolves the list of project
// IDs once at the start of the query and inlines them into the SQL
// filters. modernc/sqlite has no native array-IN binding, so we render
// the IN-list inline (safe because every value came from a config-file
// allow/denylist, not user input).
type ScopeOptions struct {
	ProjectRootAllowlist []string
	ProjectRootDenylist  []string
}

// IsScoped reports whether either list is non-empty (the query path
// must compute project IDs).
func (s ScopeOptions) IsScoped() bool {
	return len(s.ProjectRootAllowlist) > 0 || len(s.ProjectRootDenylist) > 0
}

// ShareOptions controls which content-bearing columns the org-push seam
// includes in the wire payload.
//
// The default zero value (FullContent=false, no TargetActionAllowlist) is
// the v1.8.0 metadata-only posture: only sha256-hex hashes ship for the
// content-bearing columns (target/source_file/project_root/git_remote),
// with the raw values withheld. The node operator can opt the local
// daemon into full-content sharing by setting
// [org_client.share].full_content = true in their TOML config; the org
// admin cannot force this on remotely.
//
// TargetActionAllowlist, when non-empty, restricts which action types may
// carry a raw `target` even when FullContent is false. Use this to ship
// human-readable file paths for safe action types (read_file, edit_file,
// write_file) while withholding shell command bodies (run_command) and
// assistant prose (task_complete). Empty list means: no exceptions —
// when FullContent is false, NO action ships a raw target.
type ShareOptions struct {
	FullContent           bool
	TargetActionAllowlist []string
	// AdminManaged flips the content-sharing DEFAULT for an admin-driven
	// (native-console) deployment. In that model the org admin provisions the
	// node via managed-settings/MDM and configures the telemetry collection at
	// the source, so the node-opt-in premise is inverted: all content-bearing
	// columns ship raw by default. It is still a NODE-SIDE config the admin
	// authors through the node's provisioning — there is no server-side force
	// override (the no-remote-force invariant holds). Default false, so the
	// zero value remains the metadata-only posture and the privacy invariant
	// test is unchanged. shipsRawContent() is the single place this OR's with
	// FullContent.
	AdminManaged bool
	// RoutingSummary opts the §R19.4 aggregate rollup onto the wire
	// (counts + dollars by tier/reason only — see
	// store.SelectRoutingSummaries, which owns the aggregation; this
	// file never names the underlying node-local tables). Its own
	// consent toggle (model-routing spec §R26.4), default false.
	RoutingSummary bool
	// ObsSummary / ObsTraces / ObsContent / ObsEvalSummary are the
	// org-tier observability opt-ins (obs-org-tier plan §1, the T1–T4
	// ladder). Each default false, each independent, none server-forced.
	// The underlying obs_* reads are owned by internal/obs and reached
	// ONLY through the injected obs provider seam (Store.SetObsOrgProviders)
	// — this file composes them via a function call and names no obs_*
	// table, so the privacy sentinel stays green (like RoutingSummary).
	// ObsContent additionally requires shipsRawContent() for raw bodies
	// (content_hash ships regardless).
	ObsSummary     bool // T1 aggregate
	ObsTraces      bool // T2 structure
	ObsContent     bool // T3 content bodies
	ObsEvalSummary bool // T4 eval health
	// ObsAdmission is the T6 input-admission opt-in (Plane-A admission org
	// tier, gap-audit §2.1 / #1a). Default false, node-side only, never
	// server-forced — same posture as the other obs_* opt-ins. Verdict events
	// + policy snapshots are composed via the obs provider seam; the verdict
	// PII/prose columns additionally require shipsRawContent() (policy Body
	// ships regardless).
	ObsAdmission bool // T6 input-admission
	// ObsEvalItems is the T7 per-item eval opt-in (Plane-A eval-run detail org
	// tier, gap-audit §1 / §2.2 / §6). Default false, node-side only, never
	// server-forced — same posture as the other obs_* opt-ins. Per-item scores
	// are composed via the obs provider seam; the item content excerpts
	// additionally require shipsRawContent() (score metadata + content_hash ship
	// regardless).
	ObsEvalItems bool // T7 per-item eval scores
	// ObsEgress gates the T8 egress-routing decision feed (W5.3). Default
	// false, node-side only, never server-forced. Tenant/User columns
	// additionally require shipsRawContent().
	ObsEgress bool // T8 egress routing decisions
	// FullToolBodies ships the four `actions` body columns (raw_tool_input,
	// raw_tool_output, preceding_reasoning, error_message) that the local
	// dashboard renders inline and that this seam NEVER ships in any other
	// mode. It is a DISTINCT tier from shipsRawContent() (which ships
	// paths/targets, never these bodies), so extraction is granular per the
	// enterprise-managed control model. Default false; on an individual node
	// it is node-opt-in only, on a managed node the org may RAISE it
	// (extract.managed) — the merge that can raise it lives at the ONE
	// ShareOptions construction site (internal/orgclient), never here.
	FullToolBodies bool
	// CacheDetail ships the Arc 4 P5c cache-detail aggregate (day × model ×
	// kind counts + tokens + cost delta) computed by store.SelectCacheSummaries
	// — which owns the node-local cache_events read; this file never names the
	// cache_* tables (the privacy sentinel forbids them here). Default false; on
	// an individual node node-opt-in only, on a managed node org-raisable
	// (extract.managed). The cache_* tables stay node-local except for this
	// content-free aggregate under this explicit tier.
	CacheDetail bool
	// RoutingDetail ships the Arc 4 P5d routing-detail aggregate (day ×
	// original_model × selected_model × turn_kind × mode counts + savings)
	// computed by store.SelectRoutingDetail — which owns the node-local
	// router_decisions read; this file never names that table. Distinct from
	// RoutingSummary (model-id-free tier aggregate); this tier discloses the
	// actual model ids. Default false; node-opt-in individual, org-raisable
	// (extract.managed) managed.
	RoutingDetail bool
	// LimitGauge ships the Arc 4 P5e predictions aggregate (per day × provider
	// rate-limit utilization) computed by store.SelectLimitGauges — which owns
	// the node-local limit_snapshots read; this file never names that table.
	// Content-free (utilization stats only). Default false; node-opt-in
	// individual, org-raisable (extract.managed) managed.
	LimitGauge bool
	// CodeintelDetail ships the Arc 4 P5f codeintel-detail aggregate (per
	// project-hash × language file/symbol/edge counts) computed by
	// store.SelectCodeintelSummaries — which owns the node-local codeintel_*
	// read; this file never names those tables (the privacy sentinel forbids
	// them here). Content-free STRUCTURE counts only — no symbol name, fqn,
	// signature, or raw path. Default false; on an individual node
	// node-opt-in only, on a managed node org-raisable via the DISTINCT
	// extract.codeintel authority (NOT the umbrella extract.managed — this is
	// the highest-sensitivity tier and gets its own explicit consent).
	CodeintelDetail bool
	// ProcessDetail ships the Arc 4 P5g process-detail aggregate (per day ×
	// tool run/exit/duration counts) computed by store.SelectProcessSummaries
	// — which owns the node-local process_runs read; this file never names the
	// process_* tables (the privacy sentinel forbids them here). Content-free
	// counts only — no exe path, argv, cwd, network body, or hash. Default
	// false; on an individual node node-opt-in only, on a managed node
	// org-raisable via the DISTINCT extract.process authority (NOT the umbrella
	// extract.managed — the process/eBPF trees are a highest-sensitivity tier).
	ProcessDetail bool
	// TerminalDetail ships the Arc 4 P5h terminal-detail aggregates (per
	// day×tool×kind terminal run/command counts + per day×kind×decision×principal
	// remote-audit event counts) computed by store.SelectTerminalSummaries /
	// SelectRemoteAuditSummaries — which own the node-local terminal_* /
	// remote_audit reads; this file never names those tables (the privacy
	// sentinel forbids them here, and they carry dedicated never-ships tests).
	// Content-free counts only — no command, hash, session id, peer address, or
	// route. Default false; on an individual node node-opt-in only, on a managed
	// node org-raisable via the DISTINCT extract.terminal authority. Shipping
	// this aggregate is the deliberate, reviewed reversal of the raw tables'
	// never-ships pin.
	TerminalDetail bool
}

// shipsRawContent reports whether raw content-bearing columns ship under these
// options: true when the node opted into full content (FullContent) OR when the
// node is in admin-managed mode (AdminManaged, the native-console default-flip).
// This is the ONE predicate every content-strip site consults so the two knobs
// can never diverge.
func (o ShareOptions) shipsRawContent() bool {
	return o.FullContent || o.AdminManaged
}

// shipsToolBodies reports whether the four `actions` body columns ship. It is
// deliberately its OWN predicate, NOT folded into shipsRawContent(): the body
// columns are the highest-sensitivity per-action content and were never shipped
// under FullContent/AdminManaged, so they get an independent tier the privacy
// sentinel pins separately.
func (o ShareOptions) shipsToolBodies() bool {
	return o.FullToolBodies
}

// targetAllowed reports whether the given action type may ship a raw
// target column under these options. Always true when raw content ships
// (full-content or admin-managed); otherwise true only when actionType appears
// in the allowlist (exact string match; the action_type vocabulary is
// models.ActionXxx constants like "read_file" / "edit_file" / "run_command").
func (o ShareOptions) targetAllowed(actionType string) bool {
	if o.shipsRawContent() {
		return true
	}
	for _, a := range o.TargetActionAllowlist {
		if a == actionType {
			return true
		}
	}
	return false
}

// SelectUnpushedSince reads the next batch of content-free rows above the
// given cursor, in table order (sessions, actions, api_turns, token_usage,
// guard_events), stopping once the estimated JSON size would exceed maxBytes (a single
// oversized row is still included if the batch is otherwise empty, to
// guarantee forward progress). orgID/userEmail are the enrolled identity and
// are stamped onto every row — the push is attributed to the enrolled user
// regardless of the row's locally-stored attribution columns. The returned
// Cursor carries each table's new high-water id for the rows included.
//
// Privacy posture (v1.8.0): only the allowed, content-free columns are
// selected; raw_tool_input, raw_tool_output, preceding_reasoning,
// error_message, and prompt bodies are NEVER read here. The
// content-bearing columns target / source_file / project_root / git_remote
// — and, on guard_events, reason / target_excerpt / taint_origin (guard
// spec §10.2) — are scanned (so the hash counterpart is also scanned in
// the same query), but the raw fields are zeroed in Go before the row
// enters the batch unless ShareOptions.FullContent is true (or per-action
// permitted via TargetActionAllowlist). This is the single SQL seam where
// the privacy posture is enforced (spec §1.5); the privacy invariant test
// asserts it. The remaining guard tables (guard_pins, guard_policy_state,
// guard_approvals) are NODE-LOCAL until the G13/G14 teams arc — they must
// not appear in this file (privacy sentinel enforced at the source level).
//
// share is the v1.8.0 ShareOptions; passing a zero value preserves the
// pre-v1.8 behavior would have been (raw fields shipped) but with the
// inverted default: zero value now means metadata-only. Existing callers
// that don't opt into share get the safe behavior automatically.
//
// scope is the v1.8.0 ScopeOptions; passing a zero value (both lists
// empty) means "every project ships" — the v1.7 behaviour. A non-empty
// Allowlist restricts to those project roots; a non-empty Denylist
// strips any roots matching from the result.
// nolint:gocyclo // four near-identical per-table loops (sessions, actions,
// api_turns, token_usage) each with their own SQL + per-row mapping + per-row
// privacy-strip rules; extracting helpers obscures the regular shape and
// breaks the budgetHit threading. The complexity is structural, not branchy.
func (s *Store) SelectUnpushedSince(ctx context.Context, cur PushCursor, maxBytes int64, orgID, userEmail string, share ShareOptions, scope ScopeOptions) (PushBatch, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	batch := PushBatch{Cursor: cur}
	var est int64
	budgetHit := false

	// fits reports whether a row of the given size may be added: always true
	// for the first row (forward progress), otherwise only within budget.
	fits := func(rowBytes int64) bool {
		if batch.RowCount() == 0 {
			return true
		}
		return est+rowBytes <= maxBytes
	}

	// Scope: resolve allow/deny project roots to the set of allowed
	// project_ids once, then inline the IN-list into each table's
	// WHERE clause. When neither list is set, scopeFilter == "" and the
	// queries run with no project filter. Empty allowed set after
	// resolution (e.g. allowlist had only paths that aren't in the DB)
	// means no rows are eligible — return an empty batch immediately.
	scopeFilter, scopeNoMatch, err := s.resolveScopeFilter(ctx, scope)
	if err != nil {
		return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: resolve scope: %w", err)
	}
	if scopeNoMatch {
		batch.EstBytes = 0
		return batch, nil
	}

	// --- sessions ---
	if !budgetHit {
		q := `SELECT s.rowid, s.id,
		             COALESCE(p.root_path_hash,''), COALESCE(p.git_remote_hash,''),
		             COALESCE(p.root_path,''),      COALESCE(p.git_remote,''),
		             s.tool,
		             COALESCE(s.model,''), COALESCE(s.git_branch,''), s.started_at,
		             COALESCE(s.ended_at,''), COALESCE(s.total_actions,0)
		        FROM sessions s JOIN projects p ON s.project_id = p.id
		       WHERE s.rowid > ?`
		if scopeFilter != "" {
			q += ` AND p.id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY s.rowid ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.Sessions)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: sessions: %w", err)
		}
		for rows.Next() {
			var rowid int64
			var r orgcontract.SessionRow
			if err := rows.Scan(&rowid, &r.ID,
				&r.ProjectRootHash, &r.GitRemoteHash,
				&r.ProjectRoot, &r.GitRemote,
				&r.Tool,
				&r.Model, &r.GitBranch, &r.StartedAt, &r.EndedAt, &r.TotalActions); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan session: %w", err)
			}
			// Privacy seam: strip raw paths when not opted into full-content
			// sharing. The hash counterparts (already scanned) carry the
			// signal the server needs. git_branch is stripped outright — it has
			// no hash counterpart and no server feature keys on it, yet branch
			// names routinely encode client/codename/ticket identifiers, so in
			// the default metadata-only posture it must not ship raw (it was
			// previously leaking, inconsistent with git_remote here).
			if !share.shipsRawContent() {
				r.ProjectRoot = ""
				r.GitRemote = ""
				r.GitBranch = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				budgetHit = true
				break
			}
			batch.Sessions = append(batch.Sessions, r)
			batch.Cursor.Sessions = rowid
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: sessions rows: %w", err)
		}
	}

	// --- actions ---
	if !budgetHit {
		q := `SELECT a.id, a.session_id,
		             COALESCE(a.target_hash,''), COALESCE(a.source_file_hash,''),
		             COALESCE(a.source_file,''), COALESCE(a.source_event_id,''),
		             a.timestamp, a.tool, a.action_type,
		             COALESCE(a.target,''),
		             COALESCE(a.turn_index,0), COALESCE(a.success,1), COALESCE(a.duration_ms,0),
		             COALESCE(a.is_sidechain,0),
		             COALESCE(a.raw_tool_input,''), COALESCE(a.raw_tool_output,''),
		             COALESCE(a.preceding_reasoning,''), COALESCE(a.error_message,''),
		             COALESCE(json_extract(a.metadata,'$.effort_level'),'')
		        FROM actions a WHERE a.id > ?`
		if scopeFilter != "" {
			q += ` AND a.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY a.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.Actions)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: actions: %w", err)
		}
		for rows.Next() {
			var id, success, sidechain int64
			var r orgcontract.ActionRow
			if err := rows.Scan(&id, &r.SessionID,
				&r.TargetHash, &r.SourceFileHash,
				&r.SourceFile, &r.SourceEventID,
				&r.Timestamp, &r.Tool, &r.ActionType,
				&r.Target, &r.TurnIndex,
				&success, &r.DurationMs, &sidechain,
				&r.RawToolInput, &r.RawToolOutput,
				&r.PrecedingReasoning, &r.ErrorMessage,
				&r.EffortLevel); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan action: %w", err)
			}
			r.Success = success != 0
			r.IsSidechain = sidechain != 0
			// Privacy seam:
			//   - SourceFile is a filesystem path → strip when not opted in.
			//   - Target is per-action: in full-content mode, always ship;
			//     in metadata-only mode, ship only when the action type is
			//     in the explicit TargetActionAllowlist (e.g. read_file).
			//   - The four body columns ship ONLY under the distinct
			//     shipsToolBodies() tier (never under shipsRawContent alone).
			if !share.shipsRawContent() {
				r.SourceFile = ""
			}
			if !share.targetAllowed(r.ActionType) {
				r.Target = ""
			}
			if !share.shipsToolBodies() {
				r.RawToolInput = ""
				r.RawToolOutput = ""
				r.PrecedingReasoning = ""
				r.ErrorMessage = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				budgetHit = true
				break
			}
			batch.Actions = append(batch.Actions, r)
			batch.Cursor.Actions = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: actions rows: %w", err)
		}
	}

	// --- api_turns ---
	if !budgetHit {
		q := `SELECT t.id, COALESCE(t.session_id,''),
		             COALESCE(p.root_path_hash,''), COALESCE(p.root_path,''),
		             t.timestamp,
		             t.provider, COALESCE(t.model,''), COALESCE(t.request_id,''),
		             t.input_tokens, t.output_tokens, COALESCE(t.cache_read_tokens,0),
		             COALESCE(t.cache_creation_tokens,0), COALESCE(t.cache_creation_1h_tokens,0),
		             COALESCE(t.web_search_requests,0), COALESCE(t.cost_usd,0),
		             COALESCE(t.message_count,0), COALESCE(t.tool_use_count,0),
		             COALESCE(t.system_prompt_hash,''), COALESCE(t.message_prefix_hash,''),
		             COALESCE(t.time_to_first_token_ms,0), COALESCE(t.total_response_ms,0),
		             COALESCE(t.stop_reason,''), COALESCE(t.http_status,0), COALESCE(t.error_class,'')
		        FROM api_turns t LEFT JOIN projects p ON t.project_id = p.id
		       WHERE t.id > ?`
		if scopeFilter != "" {
			q += ` AND t.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY t.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.APITurns)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: api_turns: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.APITurnRow
			if err := rows.Scan(&id, &r.SessionID,
				&r.ProjectRootHash, &r.ProjectRoot,
				&r.Timestamp, &r.Provider,
				&r.Model, &r.RequestID, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
				&r.CacheCreationTokens, &r.CacheCreation1hTokens, &r.WebSearchRequests, &r.CostUSD,
				&r.MessageCount, &r.ToolUseCount, &r.SystemPromptHash, &r.MessagePrefixHash,
				&r.TimeToFirstTokenMS, &r.TotalResponseMS, &r.StopReason, &r.HTTPStatus,
				&r.ErrorClass); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan api_turn: %w", err)
			}
			if !share.shipsRawContent() {
				r.ProjectRoot = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				budgetHit = true
				break
			}
			batch.APITurns = append(batch.APITurns, r)
			batch.Cursor.APITurns = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: api_turns rows: %w", err)
		}
	}

	// --- token_usage ---
	if !budgetHit {
		q := `SELECT tu.id, tu.session_id,
		             COALESCE(p.root_path_hash,''), COALESCE(p.root_path,''),
		             tu.timestamp, tu.tool,
		             COALESCE(tu.model,''), COALESCE(tu.input_tokens,0), COALESCE(tu.output_tokens,0),
		             COALESCE(tu.cache_read_tokens,0), COALESCE(tu.cache_creation_tokens,0),
		             COALESCE(tu.cache_creation_1h_tokens,0), COALESCE(tu.reasoning_tokens,0),
		             COALESCE(tu.web_search_requests,0), COALESCE(tu.estimated_cost_usd,0),
		             tu.source, COALESCE(tu.reliability,'unknown'),
		             COALESCE(tu.source_file_hash,''), COALESCE(tu.source_file,''),
		             COALESCE(tu.source_event_id,'')
		        FROM token_usage tu
		        LEFT JOIN sessions s ON tu.session_id = s.id
		        LEFT JOIN projects p ON s.project_id = p.id
		       WHERE tu.id > ?`
		if scopeFilter != "" {
			q += ` AND s.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY tu.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.TokenUsage)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: token_usage: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.TokenUsageRow
			if err := rows.Scan(&id, &r.SessionID,
				&r.ProjectRootHash, &r.ProjectRoot,
				&r.Timestamp, &r.Tool,
				&r.Model, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
				&r.CacheCreation1hTokens, &r.ReasoningTokens, &r.WebSearchRequests, &r.EstimatedCostUSD,
				&r.Source, &r.Reliability,
				&r.SourceFileHash, &r.SourceFile,
				&r.SourceEventID); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan token_usage: %w", err)
			}
			if !share.shipsRawContent() {
				r.ProjectRoot = ""
				r.SourceFile = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				// token_usage is the last table; no need to set budgetHit.
				break
			}
			batch.TokenUsage = append(batch.TokenUsage, r)
			batch.Cursor.TokenUsage = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: token_usage rows: %w", err)
		}
	}

	// --- guard_events (guard spec §10.2 / §14.3) ---
	// Unlike the NODE-LOCAL cache_* / advisor_* tables, guard events DO
	// push — they are the fleet-visibility surface the org rollups (G14)
	// consume. The content-bearing columns (reason, target_excerpt,
	// taint_origin) are stripped here in Go, per row, unless the node
	// opted in to full-content sharing — exactly the actions.target
	// gating. target_hash and the chain links always ship (content-free
	// sha256 hex). Local row-id anchors (action_id / api_turn_id) are
	// never selected — meaningless off-node.
	if !budgetHit {
		q := `SELECT ge.id, COALESCE(ge.session_id,''), ge.ts,
		             COALESCE(ge.tool,''), COALESCE(ge.event_kind,''), ge.rule_id,
		             COALESCE(ge.category,''), COALESCE(ge.severity,''), COALESCE(ge.decision,''),
		             COALESCE(ge.degraded_from,''), COALESCE(ge.enforced,0), COALESCE(ge.source,''),
		             COALESCE(ge.target_hash,''),
		             COALESCE(ge.reason,''), COALESCE(ge.target_excerpt,''), COALESCE(ge.taint_origin,''),
		             ge.chain_prev, ge.chain_hash
		        FROM guard_events ge
		        LEFT JOIN sessions s ON ge.session_id = s.id
		       WHERE ge.id > ?`
		if scopeFilter != "" {
			// Scope resolves through the owning session's project. A
			// guard event with no session row (e.g. a config-change
			// posture event) is conservatively EXCLUDED under a scoped
			// push — when the operator restricts by project, unattributable
			// rows don't ship.
			q += ` AND s.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY ge.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.GuardEvents)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: guard_events: %w", err)
		}
		for rows.Next() {
			var id, enforced int64
			var r orgcontract.GuardEventRow
			if err := rows.Scan(&id, &r.SessionID, &r.Timestamp,
				&r.Tool, &r.EventKind, &r.RuleID,
				&r.Category, &r.Severity, &r.Decision,
				&r.DegradedFrom, &enforced, &r.Source,
				&r.TargetHash,
				&r.Reason, &r.TargetExcerpt, &r.TaintOrigin,
				&r.ChainPrev, &r.ChainHash); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan guard_event: %w", err)
			}
			r.Enforced = enforced != 0
			// Privacy seam: strip the content-bearing verdict fields
			// when not opted into full-content sharing. The hash
			// counterpart (already scanned) carries the dedup /
			// cardinality signal the server needs.
			if !share.shipsRawContent() {
				r.Reason = ""
				r.TargetExcerpt = ""
				r.TaintOrigin = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				// guard_events is the last table; no need to set budgetHit.
				break
			}
			batch.GuardEvents = append(batch.GuardEvents, r)
			batch.Cursor.GuardEvents = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: guard_events rows: %w", err)
		}
	}

	// --- otel_content (native-console Phase 2b body-ingest) ---
	// Native-OTel content bodies (prompts, tool I/O). content_hash always
	// ships (content-free dedup/cardinality signal); the raw content ships
	// only under full-content / admin-managed sharing — the same per-row Go
	// strip as guard_events reason/excerpt. Scope resolves through the owning
	// session's project; a row with no session row is excluded under a scoped
	// push, like guard_events.
	if !budgetHit {
		q := `SELECT oc.id, COALESCE(oc.request_id,''), COALESCE(oc.session_id,''),
		             COALESCE(oc.tool_use_id,''), oc.kind, oc.content_hash,
		             COALESCE(oc.content,''), oc.timestamp
		        FROM otel_content oc
		        LEFT JOIN sessions s ON oc.session_id = s.id
		       WHERE oc.id > ?`
		if scopeFilter != "" {
			q += ` AND s.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY oc.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.OTelContent)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: otel_content: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.OTelContentRow
			if err := rows.Scan(&id, &r.RequestID, &r.SessionID,
				&r.ToolUseID, &r.Kind, &r.ContentHash,
				&r.Content, &r.Timestamp); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan otel_content: %w", err)
			}
			// Privacy seam: strip the raw body unless the node shares full
			// content (full_content / admin_managed). content_hash already
			// scanned carries the content-free signal.
			if !share.shipsRawContent() {
				r.Content = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				break
			}
			batch.OTelContent = append(batch.OTelContent, r)
			batch.Cursor.OTelContent = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: otel_content rows: %w", err)
		}
	}

	// §R19.4 aggregate rollup — attached only under the node-side
	// opt-in. Computed by store.SelectRoutingSummaries (which owns the
	// node-local read; this file deliberately never names the source
	// tables — the privacy sentinel forbids it). Counts + dollars by
	// tier/reason only.
	if share.RoutingSummary {
		sums, err := s.SelectRoutingSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: routing summaries: %w", err)
		}
		for i := range sums {
			sums[i].OrgID, sums[i].UserEmail = orgID, userEmail
			est += jsonSize(sums[i])
		}
		batch.RoutingSummaries = sums
	}

	// Cache-detail aggregate (Arc 4 P5c) — attached only under the
	// cache_detail tier. Computed by store.SelectCacheSummaries (which owns
	// the node-local cache_events read; this file deliberately never names the
	// cache_* tables — the privacy sentinel forbids it). Day × model × kind
	// counts + tokens + cost delta only, no content.
	if share.CacheDetail {
		sums, err := s.SelectCacheSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: cache summaries: %w", err)
		}
		for i := range sums {
			sums[i].OrgID, sums[i].UserEmail = orgID, userEmail
			est += jsonSize(sums[i])
		}
		batch.CacheSummaries = sums
	}

	// Session-scoped enterprise wires (org-parity W3.1 verbosity / W2.1
	// cache / W2.2 process) — attached only under shipsRawContent()
	// (FullContent || AdminManaged), deliberately NOT the teams-tier detail
	// flags: session-scoped telemetry is per-developer detail the enterprise
	// posture treats as admin-visible-by-default. Each Select lives in its
	// own file and owns its node-local table reads; this file deliberately
	// never names those tables (the privacy sentinel forbids it).
	if share.shipsRawContent() {
		sv, err := s.SelectSessionVerbositySummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: session verbosity: %w", err)
		}
		for i := range sv {
			sv[i].OrgID, sv[i].UserEmail = orgID, userEmail
			est += jsonSize(sv[i])
		}
		batch.SessionVerbositySummaries = sv

		sc, err := s.SelectSessionCacheSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: session cache summaries: %w", err)
		}
		for i := range sc {
			sc[i].OrgID, sc[i].UserEmail = orgID, userEmail
			est += jsonSize(sc[i])
		}
		batch.SessionCacheSummaries = sc

		sp, err := s.SelectSessionProcessRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: session processes: %w", err)
		}
		for i := range sp {
			sp[i].OrgID, sp[i].UserEmail = orgID, userEmail
			est += jsonSize(sp[i])
		}
		batch.SessionProcesses = sp

		sne, err := s.SelectSessionNetworkEvents(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: session network events: %w", err)
		}
		for i := range sne {
			sne[i].OrgID, sne[i].UserEmail = orgID, userEmail
			est += jsonSize(sne[i])
		}
		batch.SessionNetworkEvents = sne

		// Wave-3 per-developer enterprise wires (advisor / patterns /
		// benchmarks / compression / routing-dev / codeintel-dev /
		// terminals+remote / guard pins+approvals). Each Select lives in its
		// own file and owns its node-local table reads; this file names none
		// of them. All windowed/snapshot recomputes with idempotent server
		// upserts — no cursor effect.
		as, err := s.SelectAdvisorSuggestionRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: advisor suggestions: %w", err)
		}
		for i := range as {
			as[i].OrgID, as[i].UserEmail = orgID, userEmail
			est += jsonSize(as[i])
		}
		batch.AdvisorSuggestions = as

		pp, err := s.SelectProjectPatternRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: project patterns: %w", err)
		}
		for i := range pp {
			pp[i].OrgID, pp[i].UserEmail = orgID, userEmail
			est += jsonSize(pp[i])
		}
		batch.ProjectPatterns = pp

		bruns, batts, err := s.SelectBenchmarkOrgRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: benchmark rows: %w", err)
		}
		for i := range bruns {
			bruns[i].OrgID, bruns[i].UserEmail = orgID, userEmail
			est += jsonSize(bruns[i])
		}
		for i := range batts {
			batts[i].OrgID, batts[i].UserEmail = orgID, userEmail
			est += jsonSize(batts[i])
		}
		batch.BenchmarkRuns, batch.BenchmarkAttempts = bruns, batts

		cs, err := s.SelectCompressionStatRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: compression stats: %w", err)
		}
		for i := range cs {
			cs[i].OrgID, cs[i].UserEmail = orgID, userEmail
			est += jsonSize(cs[i])
		}
		batch.CompressionStats = cs

		rd, err := s.SelectRoutingDevRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: routing dev: %w", err)
		}
		for i := range rd {
			rd[i].OrgID, rd[i].UserEmail = orgID, userEmail
			est += jsonSize(rd[i])
		}
		batch.RoutingDevRows = rd

		cd, err := s.SelectCodeintelDevRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: codeintel dev: %w", err)
		}
		for i := range cd {
			cd[i].OrgID, cd[i].UserEmail = orgID, userEmail
			est += jsonSize(cd[i])
		}
		batch.CodeintelDevRows = cd

		tr, err := s.SelectTerminalRunRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: terminal runs: %w", err)
		}
		for i := range tr {
			tr[i].OrgID, tr[i].UserEmail = orgID, userEmail
			est += jsonSize(tr[i])
		}
		batch.TerminalRuns = tr

		tc, err := s.SelectTerminalCommandRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: terminal commands: %w", err)
		}
		for i := range tc {
			tc[i].OrgID, tc[i].UserEmail = orgID, userEmail
			est += jsonSize(tc[i])
		}
		batch.TerminalCommands = tc

		ra, err := s.SelectRemoteAuditRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: remote audit: %w", err)
		}
		for i := range ra {
			ra[i].OrgID, ra[i].UserEmail = orgID, userEmail
			est += jsonSize(ra[i])
		}
		batch.RemoteAudit = ra

		gp, err := s.SelectGuardPinRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: guard pins: %w", err)
		}
		for i := range gp {
			gp[i].OrgID, gp[i].UserEmail = orgID, userEmail
			est += jsonSize(gp[i])
		}
		batch.GuardPins = gp

		ga, err := s.SelectGuardApprovalRows(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: guard approvals: %w", err)
		}
		for i := range ga {
			ga[i].OrgID, ga[i].UserEmail = orgID, userEmail
			est += jsonSize(ga[i])
		}
		batch.GuardApprovals = ga
	}

	// Codeintel-detail aggregate (Arc 4 P5f) — attached only under the
	// codeintel_detail tier. Computed by store.SelectCodeintelSummaries (which
	// owns the node-local codeintel_* read; this file deliberately never names
	// the codeintel_* tables — the privacy sentinel forbids it). Per
	// project-hash × language file/symbol/edge STRUCTURE counts only — no
	// symbol name, fqn, signature, or raw path.
	if share.CodeintelDetail {
		sums, err := s.SelectCodeintelSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: codeintel summaries: %w", err)
		}
		for i := range sums {
			sums[i].OrgID, sums[i].UserEmail = orgID, userEmail
			est += jsonSize(sums[i])
		}
		batch.CodeintelSummaries = sums
	}

	// Process-detail aggregate (Arc 4 P5g) — attached only under the
	// process_detail tier. Computed by store.SelectProcessSummaries (which owns
	// the node-local process_runs read; this file deliberately never names the
	// process_* tables — the privacy sentinel forbids it). Per day × tool
	// run/exit/duration counts only — no exe path, argv, cwd, network body, or
	// hash.
	if share.ProcessDetail {
		sums, err := s.SelectProcessSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: process summaries: %w", err)
		}
		for i := range sums {
			sums[i].OrgID, sums[i].UserEmail = orgID, userEmail
			est += jsonSize(sums[i])
		}
		batch.ProcessSummaries = sums
	}

	// Terminal-detail aggregates (Arc 4 P5h) — attached only under the
	// terminal_detail tier. Computed by store.SelectTerminalSummaries /
	// SelectRemoteAuditSummaries (which own the node-local terminal_* /
	// remote_audit reads; this file deliberately never names those tables — the
	// privacy sentinel forbids them AND they carry dedicated never-ships tests).
	// Content-free counts only. Shipping these is the deliberate, reviewed
	// reversal of the raw tables' never-ships pin — the raw rows still never
	// cross, only these aggregates under this explicit tier.
	if share.TerminalDetail {
		tsums, err := s.SelectTerminalSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: terminal summaries: %w", err)
		}
		for i := range tsums {
			tsums[i].OrgID, tsums[i].UserEmail = orgID, userEmail
			est += jsonSize(tsums[i])
		}
		batch.TerminalSummaries = tsums

		rsums, err := s.SelectRemoteAuditSummaries(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: remote audit summaries: %w", err)
		}
		for i := range rsums {
			rsums[i].OrgID, rsums[i].UserEmail = orgID, userEmail
			est += jsonSize(rsums[i])
		}
		batch.RemoteAuditSummaries = rsums
	}

	// Routing-detail aggregate (Arc 4 P5d) — attached only under the
	// routing_detail tier. Computed by store.SelectRoutingDetail (which owns
	// the node-local router_decisions read; this file never names that table).
	// Model-id-bearing per-decision aggregate, distinct from the model-id-free
	// RoutingSummaries above.
	if share.RoutingDetail {
		dets, err := s.SelectRoutingDetail(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: routing details: %w", err)
		}
		for i := range dets {
			dets[i].OrgID, dets[i].UserEmail = orgID, userEmail
			est += jsonSize(dets[i])
		}
		batch.RoutingDetails = dets
	}

	// Predictions / limit-gauge aggregate (Arc 4 P5e) — attached only under the
	// limit_gauge tier. Computed by store.SelectLimitGauges (which owns the
	// node-local limit_snapshots read; this file never names that table).
	// Per day × provider utilization stats only, no scope/session/headers.
	if share.LimitGauge {
		gauges, err := s.SelectLimitGauges(ctx)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: limit gauges: %w", err)
		}
		for i := range gauges {
			gauges[i].OrgID, gauges[i].UserEmail = orgID, userEmail
			est += jsonSize(gauges[i])
		}
		batch.LimitGauges = gauges
	}

	// Org-tier observability rollups (obs-org-tier plan §2). Each tier is
	// reached ONLY through the injected obs provider seam — this file names
	// no obs_* table (the read lives in internal/obs/store, which owns the
	// schema), so the privacy sentinel's source-walk stays green, exactly
	// like the RoutingSummaries composition above. A nil provider (obs
	// compiled out via no_obs, or disabled) makes every tier a no-op.
	if err := s.composeObsTiers(ctx, &batch, &est, orgID, userEmail, share); err != nil {
		return PushBatch{}, err
	}

	batch.EstBytes = est
	return batch, nil
}

// composeObsTiers attaches the opt-in org-tier observability rollups to the
// batch via the injected obs provider seam (Store.obsOrg). It is the single
// composition point; it deliberately holds no obs_* table names (the reads are
// owned by internal/obs/store and returned as plain orgcontract rows). Each
// tier gates on its own ShareOptions flag AND on the provider being wired.
func (s *Store) composeObsTiers(ctx context.Context, batch *PushBatch, est *int64, orgID, userEmail string, share ShareOptions) error {
	p := s.obsOrg
	since := orgcontract.ObsCursor{SinceDay: time.Now().UTC().AddDate(0, 0, -obsOrgWindowDays).Format("2006-01-02")}
	// T1 — aggregate rollup (content-free; windowed-recompute, idempotent).
	if share.ObsSummary && p.Summaries != nil {
		rows, err := p.Summaries(ctx, obsOrgWindowDays)
		if err != nil {
			return fmt.Errorf("store.SelectUnpushedSince: obs summaries: %w", err)
		}
		for i := range rows {
			rows[i].OrgID, rows[i].UserEmail = orgID, userEmail
			*est += jsonSize(rows[i])
		}
		batch.ObsSummaries = rows
	}
	// T2 — trace/span structure (hashes only; no bodies ever).
	if err := s.composeObsSpans(ctx, batch, est, orgID, userEmail, share, since); err != nil {
		return err
	}
	// T3 — raw span content (content gated by shipsRawContent(); hash always).
	if share.ObsContent && p.Content != nil {
		rows, err := p.Content(ctx, since, obsOrgRowCap)
		if err != nil {
			return fmt.Errorf("store.SelectUnpushedSince: obs content: %w", err)
		}
		for i := range rows {
			rows[i].OrgID, rows[i].UserEmail = orgID, userEmail
			if !share.shipsRawContent() {
				rows[i].Content = "" // hash-only when raw content isn't shared
			}
			*est += jsonSize(rows[i])
		}
		batch.ObsContent = rows
	}
	// T4 — eval-run health (content-free summaries).
	if share.ObsEvalSummary && p.EvalRuns != nil {
		rows, err := p.EvalRuns(ctx, obsOrgWindowDays)
		if err != nil {
			return fmt.Errorf("store.SelectUnpushedSince: obs eval runs: %w", err)
		}
		for i := range rows {
			rows[i].OrgID, rows[i].UserEmail = orgID, userEmail
			*est += jsonSize(rows[i])
		}
		batch.ObsEvalRuns = rows
	}
	// T5 — per-end-user spend (org-budget guardrails plan §2.1). The end-user
	// id is PII, so this tier rides ONLY under ObsSummary AND shipsRawContent()
	// — mirroring how the ObsTraces ProjectRoot raw value rides under
	// ObsTraces + shipsRawContent above. Gating on ObsSummary (rather than a
	// standalone flag) keeps it a facet of the aggregate opt-in; the
	// shipsRawContent() gate is what makes the PII disclosure explicit.
	if share.ObsSummary && share.shipsRawContent() && p.EndUserSpend != nil {
		rows, err := p.EndUserSpend(ctx, obsOrgWindowDays)
		if err != nil {
			return fmt.Errorf("store.SelectUnpushedSince: obs enduser spend: %w", err)
		}
		for i := range rows {
			rows[i].OrgID, rows[i].UserEmail = orgID, userEmail
			*est += jsonSize(rows[i])
		}
		batch.ObsEndUserSpend = rows
	}
	// T6 — input-admission verdicts + policy snapshots (Plane-A admission org
	// tier, gap-audit §2.1 / #1a). Windowed like T2/T3 (since = now-window).
	// The verdict PII/prose columns (Tenant/EndUser/ReasonExcerpt) are stripped
	// here under !shipsRawContent() — mirroring the ObsContent body strip and
	// the guard reason/excerpt strip in SelectUnpushedSince; the policy Body is
	// admin-authored config and ALWAYS ships (like RoutingPolicyDoc.Body).
	if err := s.composeObsAdmission(ctx, batch, est, orgID, userEmail, share, since); err != nil {
		return err
	}
	// T7 — per-item eval scores (Plane-A eval-run detail org tier, gap-audit §1
	// / §2.2 / §6). Windowed like T2/T3/T6 (since = now-window). The item
	// content excerpts (InputExcerpt/ExpectedExcerpt/OutputExcerpt/Rationale)
	// are stripped here under !shipsRawContent() — mirroring the ObsContent body
	// strip and the admission PII/prose strip above; the score metadata and the
	// content_hash always ship.
	if err := s.composeObsEvalItems(ctx, batch, est, orgID, userEmail, share, since); err != nil {
		return err
	}
	if err := s.composeObsEgress(ctx, batch, est, orgID, userEmail, share, since); err != nil {
		return err
	}
	return nil
}

// composeObsSpans attaches the T2 trace/span structure tier (hashes only; no
// bodies). Gated on ObsTraces + the Spans provider being wired; project_root
// raw values ride only under shipsRawContent(). Extracted from composeObsTiers
// to keep that function's cyclomatic complexity in check.
func (s *Store) composeObsSpans(ctx context.Context, batch *PushBatch, est *int64, orgID, userEmail string, share ShareOptions, since orgcontract.ObsCursor) error {
	if !share.ObsTraces || s.obsOrg.Spans == nil {
		return nil
	}
	sb, err := s.obsOrg.Spans(ctx, since, obsOrgRowCap)
	if err != nil {
		return fmt.Errorf("store.SelectUnpushedSince: obs spans: %w", err)
	}
	for i := range sb.Traces {
		sb.Traces[i].OrgID, sb.Traces[i].UserEmail = orgID, userEmail
		// project_root raw value rides only under shipsRawContent().
		if !share.shipsRawContent() {
			sb.Traces[i].ProjectRoot = ""
		}
		*est += jsonSize(sb.Traces[i])
	}
	for i := range sb.Spans {
		sb.Spans[i].OrgID, sb.Spans[i].UserEmail = orgID, userEmail
		*est += jsonSize(sb.Spans[i])
	}
	for i := range sb.Events {
		sb.Events[i].OrgID, sb.Events[i].UserEmail = orgID, userEmail
		*est += jsonSize(sb.Events[i])
	}
	batch.ObsTraces, batch.ObsSpans, batch.ObsSpanEvents = sb.Traces, sb.Spans, sb.Events
	return nil
}

// composeObsAdmission attaches the T6 input-admission verdicts + policy
// snapshots tier. Gated on ObsAdmission + the Admission provider. The verdict
// PII/prose columns (Tenant/EndUser/ReasonExcerpt) are stripped under
// !shipsRawContent(); the admin-authored policy Body always ships. Extracted
// from composeObsTiers to keep that function's cyclomatic complexity in check.
func (s *Store) composeObsAdmission(ctx context.Context, batch *PushBatch, est *int64, orgID, userEmail string, share ShareOptions, since orgcontract.ObsCursor) error {
	if !share.ObsAdmission || s.obsOrg.Admission == nil {
		return nil
	}
	ab, err := s.obsOrg.Admission(ctx, since, obsOrgRowCap)
	if err != nil {
		return fmt.Errorf("store.SelectUnpushedSince: obs admission: %w", err)
	}
	for i := range ab.Events {
		ab.Events[i].OrgID, ab.Events[i].UserEmail = orgID, userEmail
		if !share.shipsRawContent() {
			ab.Events[i].Tenant = ""
			ab.Events[i].EndUser = ""
			ab.Events[i].ReasonExcerpt = ""
		}
		*est += jsonSize(ab.Events[i])
	}
	for i := range ab.Policies {
		ab.Policies[i].OrgID, ab.Policies[i].UserEmail = orgID, userEmail
		*est += jsonSize(ab.Policies[i])
	}
	batch.ObsAdmissionEvents = ab.Events
	batch.ObsAdmissionPolicies = ab.Policies
	return nil
}

// composeObsEvalItems attaches the T7 per-item eval scores tier. Gated on
// ObsEvalItems + the EvalItems provider. The item content excerpts are stripped
// under !shipsRawContent(); score metadata + content_hash always ship. Extracted
// from composeObsTiers to keep that function's cyclomatic complexity in check.
func (s *Store) composeObsEvalItems(ctx context.Context, batch *PushBatch, est *int64, orgID, userEmail string, share ShareOptions, since orgcontract.ObsCursor) error {
	if !share.ObsEvalItems || s.obsOrg.EvalItems == nil {
		return nil
	}
	eb, err := s.obsOrg.EvalItems(ctx, since, obsOrgRowCap)
	if err != nil {
		return fmt.Errorf("store.SelectUnpushedSince: obs eval items: %w", err)
	}
	for i := range eb.Items {
		eb.Items[i].OrgID, eb.Items[i].UserEmail = orgID, userEmail
		if !share.shipsRawContent() {
			eb.Items[i].InputExcerpt = ""
			eb.Items[i].ExpectedExcerpt = ""
			eb.Items[i].OutputExcerpt = ""
			eb.Items[i].Rationale = ""
		}
		*est += jsonSize(eb.Items[i])
	}
	batch.ObsEvalItems = eb.Items
	return nil
}

// composeObsEgress attaches the T8 egress-routing decision feed (W5.3).
// Gated on ObsEgress + the Egress provider. Tenant/User (the only
// PII-shaped columns) are stripped under !shipsRawContent(); everything
// else ships whenever the tier is on, mirroring composeObsAdmission.
func (s *Store) composeObsEgress(ctx context.Context, batch *PushBatch, est *int64, orgID, userEmail string, share ShareOptions, since orgcontract.ObsCursor) error {
	if !share.ObsEgress || s.obsOrg.Egress == nil {
		return nil
	}
	eb, err := s.obsOrg.Egress(ctx, since, obsOrgRowCap)
	if err != nil {
		return fmt.Errorf("store.SelectUnpushedSince: obs egress: %w", err)
	}
	for i := range eb.Events {
		eb.Events[i].OrgID, eb.Events[i].UserEmail = orgID, userEmail
		if !share.shipsRawContent() {
			eb.Events[i].Tenant = ""
			eb.Events[i].User = ""
		}
		*est += jsonSize(eb.Events[i])
	}
	batch.ObsEgressDecisions = eb.Events
	return nil
}

// resolveScopeFilter turns the configured project_root allowlist /
// denylist into a SQL fragment that can be AND-ed into per-table WHERE
// clauses. Returns:
//
//   - filter: an empty string when no scope is configured (everything
//     ships), or e.g. "AND p.id IN (1,3,5)" otherwise. The caller's
//     queries use alias `p` for the projects table; for the actions
//     and api_turns paths, swap `p.id` for `a.project_id` /
//     `t.project_id` directly.
//   - noMatch: true when an allowlist was configured but no project
//     root in the DB matches; the caller should short-circuit to an
//     empty batch (the operator asked for nothing).
//   - err: a real I/O error reading the projects table.
//
// Values come from a config-file allow/denylist (not user input), so
// the IN-list is rendered inline — modernc/sqlite has no native array
// binding and a single per-token bind would explode for hundreds of
// roots. Project IDs are integers, so injection is impossible by
// construction.
func (s *Store) resolveScopeFilter(ctx context.Context, scope ScopeOptions) (filter string, noMatch bool, err error) {
	if !scope.IsScoped() {
		return "", false, nil
	}
	roots, err := s.projectIDsByRoot(ctx)
	if err != nil {
		return "", false, err
	}
	var ids []int64
	if len(scope.ProjectRootAllowlist) > 0 {
		seen := make(map[int64]bool)
		for _, rp := range scope.ProjectRootAllowlist {
			if id, ok := roots[rp]; ok && !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		if len(ids) == 0 {
			return "", true, nil
		}
	} else {
		// No allowlist → start from every project, then subtract denied.
		for _, id := range roots {
			ids = append(ids, id)
		}
	}
	if len(scope.ProjectRootDenylist) > 0 {
		denied := make(map[int64]bool)
		for _, rp := range scope.ProjectRootDenylist {
			if id, ok := roots[rp]; ok {
				denied[id] = true
			}
		}
		filtered := ids[:0]
		for _, id := range ids {
			if !denied[id] {
				filtered = append(filtered, id)
			}
		}
		ids = filtered
		if len(ids) == 0 {
			return "", true, nil
		}
	}
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String(), false, nil
}

// projectIDsByRoot returns the {root_path → project_id} map.
func (s *Store) projectIDsByRoot(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, root_path FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id int64
		var rp string
		if err := rows.Scan(&id, &rp); err != nil {
			return nil, err
		}
		out[rp] = id
	}
	return out, rows.Err()
}

// jsonSize returns the marshalled byte length of v, used to budget a batch.
func jsonSize(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// closeRows closes rows and returns the first of any iteration or close error.
func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
}

// readMeta returns the schema_meta value for key, or "" if absent.
func (s *Store) readMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}
