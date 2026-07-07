package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// hashOrComputed returns the supplied hash when non-empty (the v1.8.0
// metadata-only client computed it locally and shipped it) or computes
// sha256-hex of raw when the hash field is absent (a v1.7.x client that
// shipped only the raw value). Empty raw with empty hash returns "" —
// the field simply isn't present in this row.
func hashOrComputed(hashField, raw string) string {
	if hashField != "" {
		return hashField
	}
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// dedupKey returns the value to bind to the PRIMARY KEY slot when an
// agent ships either the raw path (v1.7.x or v1.8.0 full-content mode)
// or the hash only (v1.8.0 metadata-only mode). Prefer raw when present
// so v1.7.x → v1.8.0 server pushes keep their existing PK rows; fall
// back to the hash so a metadata-only push still has a unique key.
func dedupKey(raw, hashField string) string {
	if raw != "" {
		return raw
	}
	return hashField
}

// guardDedupKey returns the chain_hash PK component for a guard-event
// row. Guard rows carry no (source_file, source_event_id) natural key;
// the §10.4 tamper-evidence chain_hash is unique per event per node,
// so (chain_hash, user_id) is the dedup key. Every real agent stamps
// chain_hash, but the compat posture (the hashOrComputed precedent)
// tolerates its absence: a row arriving without one gets a
// deterministic synthesized hash over its content-free identity
// fields, so a re-push of the same row still dedups instead of
// erroring on an empty PK.
func guardDedupKey(g orgcontract.GuardEventRow) string {
	if g.ChainHash != "" {
		return g.ChainHash
	}
	sum := sha256.Sum256([]byte("sbo-guard-dedup-v1\x00" + g.SessionID + "\x00" + g.Timestamp +
		"\x00" + g.RuleID + "\x00" + g.Decision + "\x00" + g.TargetHash + "\x00" + g.ChainPrev))
	return hex.EncodeToString(sum[:])
}

// Result reports how a push batch landed: rows newly stored vs rows already
// present (deduplicated by composite key).
type Result struct {
	Accepted int64
	Deduped  int64
}

// Push ingests a push envelope into the server data tables under one
// transaction. Every row is attributed to userID (the dedup-key user_id
// component and pushed_by_user_id) and stamped pushedAt. Rows whose composite
// key already exists are ignored. user_id AND user_email are always the
// authenticated pusher, never client-supplied (see forcePusherEmail).
func Push(ctx context.Context, db *sql.DB, env orgcontract.PushEnvelope, userID, pushedAt string) (Result, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("ingest.Push: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Pin every row's user_email to the pusher's own SCIM-provisioned address,
	// exactly as user_id is already pinned. The agent stamps user_email at read
	// time; without this a malicious enrolled agent could set another
	// developer's email and overwrite or misattribute their AGGREGATE rows,
	// whose ON CONFLICT keys are (org_id, user_email, ...) rather than user_id
	// (routing_summaries / obs_summaries / obs_eval_runs; the obs trace tables
	// carry it too). Resolve-failure is fail-open — the pusher is still
	// bearer-authenticated and the high-value core tables are user_id-pinned,
	// so refusing all ingest would be a worse outcome than the residual.
	if email := pusherEmail(ctx, tx, userID); email != "" {
		forcePusherEmail(&env, email)
	}

	var res Result
	total := int64(len(env.Sessions) + len(env.Actions) + len(env.APITurns) + len(env.TokenUsage) + len(env.GuardEvents) + len(env.OTelContent))

	for _, s := range env.Sessions {
		projRootHash := hashOrComputed(s.ProjectRootHash, s.ProjectRoot)
		gitRemoteHash := hashOrComputed(s.GitRemoteHash, s.GitRemote)
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO sessions
			   (id, user_id, org_id, user_email,
			    project_root, project_root_hash, git_remote, git_remote_hash,
			    tool, model, git_branch, started_at, ended_at, total_actions,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.ID, userID, s.OrgID, s.UserEmail,
			nullIfEmpty(s.ProjectRoot), nullIfEmpty(projRootHash),
			nullIfEmpty(s.GitRemote), nullIfEmpty(gitRemoteHash),
			s.Tool, s.Model, s.GitBranch, s.StartedAt, nullIfEmpty(s.EndedAt), s.TotalActions,
			pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: session %s: %w", s.ID, err)
		}
		res.Accepted += n
	}

	for _, a := range env.Actions {
		targetHash := hashOrComputed(a.TargetHash, a.Target)
		srcFileHash := hashOrComputed(a.SourceFileHash, a.SourceFile)
		// PK on (source_file, source_event_id, user_id) — fall back to
		// the hash when the agent didn't ship a raw source_file (v1.8.0
		// metadata-only mode). Hashes are unique, so dedup still holds.
		//
		// Naming note (N5 in docs/teams-test-regression-2026-06-03.md):
		// the column is literally `actions.source_file` even in
		// metadata-only mode, where it holds the sha256 hex of the raw
		// path rather than the path itself. Reading the DB directly,
		// the value type is recoverable via length (64 hex chars =
		// hash; anything else = raw path) and the sibling
		// `source_file_hash` column (always populated since v1.8.0).
		// Migration files are immutable once shipped, so renaming
		// would require a new ALTER + ingest rewrite — not worth it
		// when downstream queries (dedup, scope, dashboard) all treat
		// the column as an opaque PK key.
		pkSourceFile := dedupKey(a.SourceFile, srcFileHash)
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO actions
			   (source_file, source_event_id, user_id, session_id, org_id, user_email, timestamp,
			    tool, action_type, target, target_hash, source_file_hash,
			    turn_index, success, duration_ms, is_sidechain,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			pkSourceFile, a.SourceEventID, userID, a.SessionID, a.OrgID, a.UserEmail, a.Timestamp,
			a.Tool, a.ActionType, nullIfEmpty(a.Target), nullIfEmpty(targetHash), nullIfEmpty(srcFileHash),
			a.TurnIndex, boolToInt(a.Success), a.DurationMs,
			boolToInt(a.IsSidechain), pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: action %s/%s: %w", pkSourceFile, a.SourceEventID, err)
		}
		res.Accepted += n
	}

	for _, t := range env.APITurns {
		projRootHash := hashOrComputed(t.ProjectRootHash, t.ProjectRoot)
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO api_turns
			   (user_id, org_id, user_email, session_id, project_root, project_root_hash, timestamp,
			    provider, model, request_id, input_tokens, output_tokens, cache_read_tokens,
			    cache_creation_tokens, cache_creation_1h_tokens, web_search_requests, cost_usd,
			    message_count, tool_use_count, system_prompt_hash, message_prefix_hash,
			    time_to_first_token_ms, total_response_ms,
			    stop_reason, http_status, error_class, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			userID, t.OrgID, t.UserEmail, t.SessionID, nullIfEmpty(t.ProjectRoot), nullIfEmpty(projRootHash),
			t.Timestamp, t.Provider, t.Model,
			t.RequestID, t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheCreationTokens,
			t.CacheCreation1hTokens, t.WebSearchRequests, t.CostUSD, t.MessageCount, t.ToolUseCount,
			t.SystemPromptHash, t.MessagePrefixHash, t.TimeToFirstTokenMS, t.TotalResponseMS,
			t.StopReason, t.HTTPStatus, t.ErrorClass, pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: api_turn %s: %w", t.RequestID, err)
		}
		res.Accepted += n
	}

	for _, u := range env.TokenUsage {
		projRootHash := hashOrComputed(u.ProjectRootHash, u.ProjectRoot)
		srcFileHash := hashOrComputed(u.SourceFileHash, u.SourceFile)
		pkSourceFile := dedupKey(u.SourceFile, srcFileHash)
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO token_usage
			   (source_file, source_event_id, user_id, org_id, user_email, session_id,
			    project_root, project_root_hash, source_file_hash,
			    timestamp, tool, model, input_tokens, output_tokens, cache_read_tokens,
			    cache_creation_tokens, cache_creation_1h_tokens, reasoning_tokens, web_search_requests,
			    estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			pkSourceFile, u.SourceEventID, userID, u.OrgID, u.UserEmail, u.SessionID,
			nullIfEmpty(u.ProjectRoot), nullIfEmpty(projRootHash), nullIfEmpty(srcFileHash),
			u.Timestamp, u.Tool, u.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens,
			u.CacheCreationTokens, u.CacheCreation1hTokens, u.ReasoningTokens, u.WebSearchRequests,
			u.EstimatedCostUSD, u.Source, u.Reliability, pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: token_usage %s/%s: %w", pkSourceFile, u.SourceEventID, err)
		}
		res.Accepted += n
	}

	// §R19.4 routing aggregate — optional both directions (older
	// agents omit it; the rows UPSERT by natural key so re-pushed
	// windows are idempotent). Counted as accepted rows but excluded
	// from the dedup math (an upsert refresh is not a dupe).
	for _, rs := range env.RoutingSummaries {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO routing_summaries
			   (org_id, user_email, day, tier, reason, mode,
			    decisions, applied, est_savings_usd, cache_forfeit_usd,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(org_id, user_email, day, tier, reason, mode) DO UPDATE SET
			   decisions = excluded.decisions,
			   applied = excluded.applied,
			   est_savings_usd = excluded.est_savings_usd,
			   cache_forfeit_usd = excluded.cache_forfeit_usd,
			   pushed_at = excluded.pushed_at`,
			rs.OrgID, rs.UserEmail, rs.Day, rs.Tier, rs.Reason, rs.Mode,
			rs.Decisions, rs.Applied, rs.EstSavingsUSD, rs.CacheForfeitUSD,
			pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: routing_summary %s/%s: %w", rs.Day, rs.Tier, err)
		}
	}

	// Guard-layer verdict rows (guard spec §14.3, G14). The
	// content-bearing columns (reason, target_excerpt, taint_origin)
	// are empty unless the node opted into full-content sharing — the
	// agent's SelectUnpushedSince strips them per §10.2 — and are
	// stored NULL so the server cannot tell "stripped" from "never
	// had one" (no posture leak). Pre-guard agents simply send no
	// guard_events key (omitempty), so this loop is a no-op for them.
	for _, g := range env.GuardEvents {
		key := guardDedupKey(g)
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO guard_events
			   (chain_hash, user_id, org_id, user_email, session_id, timestamp,
			    tool, event_kind, rule_id, category, severity, decision,
			    degraded_from, enforced, source, target_hash,
			    reason, target_excerpt, taint_origin, chain_prev,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			key, userID, g.OrgID, g.UserEmail, nullIfEmpty(g.SessionID), g.Timestamp,
			g.Tool, g.EventKind, g.RuleID, g.Category, g.Severity, g.Decision,
			nullIfEmpty(g.DegradedFrom), boolToInt(g.Enforced), g.Source, nullIfEmpty(g.TargetHash),
			nullIfEmpty(g.Reason), nullIfEmpty(g.TargetExcerpt), nullIfEmpty(g.TaintOrigin), g.ChainPrev,
			pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: guard_event %s: %w", key, err)
		}
		res.Accepted += n
	}

	// Native-OTel content bodies (native-console Phase 2b, v1.8.x+). content
	// is empty unless the node shares full content — the agent's
	// SelectUnpushedSince strips it per the content gate — and is stored NULL
	// so the server cannot tell "stripped" from "never had one". Pre-feature
	// agents send no otel_content key (omitempty), so this loop is a no-op.
	for _, oc := range env.OTelContent {
		hash := hashOrComputed(oc.ContentHash, oc.Content)
		if hash == "" {
			continue // nothing to anchor a dedup key on
		}
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO otel_content
			   (content_hash, user_id, org_id, user_email, request_id, session_id,
			    tool_use_id, kind, content, timestamp, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			hash, userID, oc.OrgID, oc.UserEmail, oc.RequestID, nullIfEmpty(oc.SessionID),
			oc.ToolUseID, nullIfEmpty(oc.Kind), nullIfEmpty(oc.Content), oc.Timestamp,
			pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: otel_content %s: %w", hash, err)
		}
		res.Accepted += n
	}

	if err := ingestObsTiers(ctx, tx, env, userID, pushedAt); err != nil {
		return Result{}, err
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("ingest.Push: commit: %w", err)
	}
	res.Deduped = total - res.Accepted
	return res, nil
}

// ingestObsTiers writes the opt-in org-tier observability rollups (obs-org-tier
// plan §5.2). Each tier UPSERTs by its composite natural key so a re-pushed
// window (the windowed-recompute model) is idempotent. All slices are omitempty
// — a node not opted into a tier sends nothing and the loop is a no-op. Content
// (T3) stores NULL when empty so the server cannot tell "stripped" from "never
// had one" (no posture leak), exactly like otel_content.
func ingestObsTiers(ctx context.Context, tx *sql.Tx, env orgcontract.PushEnvelope, userID, pushedAt string) error {
	// T1 — aggregate rollup.
	for _, r := range env.ObsSummaries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO obs_summaries
			   (org_id, user_email, day, model, provider, project_hash, source,
			    traces, spans, input_tokens, output_tokens, cache_read_tokens,
			    cache_write_tokens, reasoning_tokens, total_tokens, cost_usd,
			    error_traces, duration_ms_sum, duration_ms_count,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(org_id, user_email, day, model, provider, project_hash, source) DO UPDATE SET
			   traces=excluded.traces, spans=excluded.spans,
			   input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			   cache_read_tokens=excluded.cache_read_tokens, cache_write_tokens=excluded.cache_write_tokens,
			   reasoning_tokens=excluded.reasoning_tokens, total_tokens=excluded.total_tokens,
			   cost_usd=excluded.cost_usd, error_traces=excluded.error_traces,
			   duration_ms_sum=excluded.duration_ms_sum, duration_ms_count=excluded.duration_ms_count,
			   pushed_at=excluded.pushed_at`,
			r.OrgID, r.UserEmail, r.Day, r.Model, r.Provider, r.ProjectHash, r.Source,
			r.Traces, r.Spans, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
			r.CacheWriteTokens, r.ReasoningTokens, r.TotalTokens, r.CostUSD,
			r.ErrorTraces, r.DurationMsSum, r.DurationMsCount, pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_summary %s/%s: %w", r.Day, r.Model, err)
		}
	}
	// T2 — trace structure.
	for _, r := range env.ObsTraces {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO obs_traces
			   (org_id, trace_id, user_email, session_id, thread_id, source, status,
			    started_at, ended_at, project_hash, project_root, root_span_id,
			    span_count, total_tokens, cost_usd, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(org_id, trace_id) DO UPDATE SET
			   session_id=excluded.session_id, thread_id=excluded.thread_id,
			   source=excluded.source, status=excluded.status,
			   started_at=excluded.started_at, ended_at=excluded.ended_at,
			   project_hash=excluded.project_hash, project_root=excluded.project_root,
			   root_span_id=excluded.root_span_id, span_count=excluded.span_count,
			   total_tokens=excluded.total_tokens, cost_usd=excluded.cost_usd,
			   pushed_at=excluded.pushed_at`,
			r.OrgID, r.TraceID, r.UserEmail, nullIfEmpty(r.SessionID), nullIfEmpty(r.ThreadID),
			r.Source, r.Status, r.StartedAt, nullIfEmpty(r.EndedAt), r.ProjectHash,
			nullIfEmpty(r.ProjectRoot), nullIfEmpty(r.RootSpanID), r.SpanCount, r.TotalTokens,
			r.CostUSD, pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_trace %s: %w", r.TraceID, err)
		}
	}
	// T2 — spans.
	for _, r := range env.ObsSpans {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO obs_spans
			   (org_id, trace_id, span_id, user_email, parent_span_id, kind, name,
			    started_at, ended_at, duration_ms, status, model, provider,
			    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			    reasoning_tokens, total_tokens, cost_usd, cost_source, request_id,
			    tool_call_id, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(org_id, trace_id, span_id) DO UPDATE SET
			   parent_span_id=excluded.parent_span_id, kind=excluded.kind, name=excluded.name,
			   started_at=excluded.started_at, ended_at=excluded.ended_at, duration_ms=excluded.duration_ms,
			   status=excluded.status, model=excluded.model, provider=excluded.provider,
			   input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
			   cache_read_tokens=excluded.cache_read_tokens, cache_write_tokens=excluded.cache_write_tokens,
			   reasoning_tokens=excluded.reasoning_tokens, total_tokens=excluded.total_tokens,
			   cost_usd=excluded.cost_usd, cost_source=excluded.cost_source, request_id=excluded.request_id,
			   tool_call_id=excluded.tool_call_id, pushed_at=excluded.pushed_at`,
			r.OrgID, r.TraceID, r.SpanID, r.UserEmail, nullIfEmpty(r.ParentSpanID), r.Kind, nullIfEmpty(r.Name),
			r.StartedAt, nullIfEmpty(r.EndedAt), r.DurationMs, r.Status, nullIfEmpty(r.Model), nullIfEmpty(r.Provider),
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
			r.ReasoningTokens, r.TotalTokens, r.CostUSD, nullIfEmpty(r.CostSource), nullIfEmpty(r.RequestID),
			nullIfEmpty(r.ToolCallID), pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_span %s/%s: %w", r.TraceID, r.SpanID, err)
		}
	}
	// T2 — span events.
	for _, r := range env.ObsSpanEvents {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO obs_span_events
			   (org_id, trace_id, span_id, user_email, time, name, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?)`,
			r.OrgID, r.TraceID, r.SpanID, r.UserEmail, r.Time, r.Name, pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_span_event %s/%s: %w", r.SpanID, r.Name, err)
		}
	}
	// T3 — span content (content NULL unless shared; hash always).
	for _, r := range env.ObsContent {
		hash := hashOrComputed(r.ContentHash, r.Content)
		if hash == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO obs_content
			   (org_id, content_hash, kind, span_id, user_email, trace_id, content,
			    timestamp, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			r.OrgID, hash, nullIfEmpty(r.Kind), r.SpanID, r.UserEmail, nullIfEmpty(r.TraceID),
			nullIfEmpty(r.Content), r.Timestamp, pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_content %s: %w", hash, err)
		}
	}
	// T4 — eval-run health.
	for _, r := range env.ObsEvalRuns {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO obs_eval_summaries
			   (org_id, user_email, day, dataset_name, run_name, scorer_name, source,
			    total, passed, mean_score, min_score, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(org_id, user_email, day, dataset_name, run_name, scorer_name, source) DO UPDATE SET
			   total=excluded.total, passed=excluded.passed, mean_score=excluded.mean_score,
			   min_score=excluded.min_score, pushed_at=excluded.pushed_at`,
			r.OrgID, r.UserEmail, r.Day, r.DatasetName, r.RunName, r.ScorerName, r.Source,
			r.Total, r.Passed, r.MeanScore, r.MinScore, pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_eval_summary %s/%s: %w", r.Day, r.RunName, err)
		}
	}
	// T5 — per-end-user spend. user_email is re-pinned to the authenticated
	// pusher (via forcePusherEmail, like every other content-bearing slice);
	// end_user is app-shared and is NOT re-pinned. The natural key includes
	// user_email so each node's per-(day, end_user) row stays distinct, and
	// the rollup SUMs across nodes for the cross-instance total.
	for _, r := range env.ObsEndUserSpend {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO obs_enduser_spend
			   (org_id, user_email, day, end_user, cost_usd, traces, total_tokens,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(org_id, user_email, day, end_user) DO UPDATE SET
			   cost_usd=excluded.cost_usd, traces=excluded.traces,
			   total_tokens=excluded.total_tokens, pushed_at=excluded.pushed_at`,
			r.OrgID, r.UserEmail, r.Day, r.EndUser, r.CostUSD, r.Traces, r.TotalTokens,
			pushedAt, userID); err != nil {
			return fmt.Errorf("ingest.Push: obs_enduser_spend %s/%s: %w", r.Day, r.EndUser, err)
		}
	}
	return nil
}

// exec runs an INSERT OR IGNORE and returns 1 if a row was inserted, 0 if the
// composite key already existed (the dedup signal).
func exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	r, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pusherEmail resolves the authenticated pusher's SCIM-provisioned email from
// org_members. Empty on any error or unknown user (fail-open: the caller then
// leaves the agent-stamped user_email untouched).
func pusherEmail(ctx context.Context, tx *sql.Tx, userID string) string {
	var email string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM org_members WHERE user_id = ?`, userID).Scan(&email); err != nil {
		return ""
	}
	return email
}

// forcePusherEmail overwrites the user_email on every row of env with the
// authenticated pusher's address. A new content-bearing slice added to
// PushEnvelope MUST be added here too, so its user_email cannot be forged.
func forcePusherEmail(env *orgcontract.PushEnvelope, email string) {
	for i := range env.Sessions {
		env.Sessions[i].UserEmail = email
	}
	for i := range env.Actions {
		env.Actions[i].UserEmail = email
	}
	for i := range env.APITurns {
		env.APITurns[i].UserEmail = email
	}
	for i := range env.TokenUsage {
		env.TokenUsage[i].UserEmail = email
	}
	for i := range env.RoutingSummaries {
		env.RoutingSummaries[i].UserEmail = email
	}
	for i := range env.GuardEvents {
		env.GuardEvents[i].UserEmail = email
	}
	for i := range env.OTelContent {
		env.OTelContent[i].UserEmail = email
	}
	for i := range env.ObsSummaries {
		env.ObsSummaries[i].UserEmail = email
	}
	for i := range env.ObsTraces {
		env.ObsTraces[i].UserEmail = email
	}
	for i := range env.ObsSpans {
		env.ObsSpans[i].UserEmail = email
	}
	for i := range env.ObsSpanEvents {
		env.ObsSpanEvents[i].UserEmail = email
	}
	for i := range env.ObsContent {
		env.ObsContent[i].UserEmail = email
	}
	for i := range env.ObsEvalRuns {
		env.ObsEvalRuns[i].UserEmail = email
	}
	// end_user is deliberately NOT re-pinned (it is the app-shared end-user
	// identity, not the pusher); only user_email is forced to the pusher.
	for i := range env.ObsEndUserSpend {
		env.ObsEndUserSpend[i].UserEmail = email
	}
}
