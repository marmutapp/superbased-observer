package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/verbosity"
)

// isoSinceDays returns the RFC3339 timestamp `days` days before now (UTC),
// for the aggregate window filter on sessions.started_at.
func isoSinceDays(days int) string {
	return time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
}

// verbosity.go is the SQL seam for the Output Composition (Verbosity)
// feature (docs/plans/output-composition-verbosity-plan-2026-06-30.md). It
// assembles a verbosity.Breakdown from existing NODE-LOCAL rows — the pure
// internal/verbosity package owns all classification/rollup math, this file
// only fetches plain data and folds it in. It writes nothing.
//
// Two substrates, both already captured:
//   - Visible text: assistant-text rows carry the scrubbed body in
//     actions.raw_tool_output; SegmentVisible runs at read time (prose vs
//     fenced artifacts). The body is scrubbed + capped at 1 MiB by the
//     adapter, so byte counts are exact for prose and only approximate for
//     a single >1 MiB message (rare) — honest and documented.
//   - Authored code: file-write/edit and run_command rows carry the authored
//     byte length in actions.content_bytes (migration 054, computed from the
//     untruncated tool input at ingest).

// assistantTextFilter selects assistant spoken-text rows. The raw_tool_name
// convention "<tool>.assistant_text" is the capability marker (claude-code
// emits "claudecode.assistant_text"); LIKE keeps it forward-compatible with
// other adapters adopting the same convention.
const assistantTextFilter = `raw_tool_name LIKE '%assistant_text%'`

// LoadSessionVerbosity assembles the output-composition Breakdown for one
// session.
func (s *Store) LoadSessionVerbosity(ctx context.Context, sessionID string) (*verbosity.Breakdown, error) {
	b := verbosity.NewBreakdown()
	if err := s.foldVisible(ctx, sessionID, b); err != nil {
		return nil, err
	}
	if err := s.foldAuthored(ctx, sessionID, b); err != nil {
		return nil, err
	}
	return b, nil
}

// foldVisible segments every assistant-text body in the session into the
// breakdown's narrative/artifact buckets.
func (s *Store) foldVisible(ctx context.Context, sessionID string, b *verbosity.Breakdown) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT raw_tool_output FROM actions
		 WHERE session_id = ? AND `+assistantTextFilter+`
		   AND raw_tool_output IS NOT NULL AND raw_tool_output != ''`,
		sessionID)
	if err != nil {
		return fmt.Errorf("store.LoadSessionVerbosity: visible: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var body sql.NullString
		if err := rows.Scan(&body); err != nil {
			return fmt.Errorf("store.LoadSessionVerbosity: visible scan: %w", err)
		}
		if body.Valid {
			b.AddVisibleText(body.String)
		}
	}
	return rows.Err()
}

// UnknownLedger is the §4 close-out evidence: the file extensions (from
// write/edit action targets) and fence tags (from assistant text) that the
// verbosity language table does NOT yet resolve, with how often each occurs.
// Drives extending the FileType/tag maps from REAL misses, not guesses.
type UnknownLedger struct {
	Extensions map[string]int64 // raw extension → count of write/edit actions
	FenceTags  map[string]int64 // unrecognized fence tag → bytes
	// TotalWrites / ResolvedWrites let the caller report the unknown-ext
	// RATE (the §4 acceptance is ≤1% of write/edit actions unresolved).
	TotalWrites    int64
	ResolvedWrites int64
}

// LoadUnknownLedger scans write/edit action targets and assistant-text fence
// tags across the window (sinceDays<=0 = all) and reports what the language
// table fails to resolve. It is independent of actions.content_bytes (uses
// only the target path), so it works on historical data immediately. It
// reuses the pure Breakdown classifiers — every known target lands in
// Written (ignored here) and every unknown one in WrittenUnknownExt.
func (s *Store) LoadUnknownLedger(ctx context.Context, sinceDays int) (UnknownLedger, error) {
	b := verbosity.NewBreakdown()
	where := ""
	args := []any{models.ActionWriteFile, models.ActionEditFile}
	if sinceDays > 0 {
		where = " AND a.timestamp >= ?"
		args = append(args, isoSinceDays(sinceDays))
	}

	var total, resolved int64
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(a.target, '') FROM actions a
		  WHERE a.action_type IN (?, ?) AND a.target IS NOT NULL AND a.target != ''`+where,
		args...)
	if err != nil {
		return UnknownLedger{}, fmt.Errorf("store.LoadUnknownLedger: ext: %w", err)
	}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			rows.Close()
			return UnknownLedger{}, fmt.Errorf("store.LoadUnknownLedger: ext scan: %w", err)
		}
		total++
		if _, ok := verbosity.FileType(target); ok {
			resolved++
		} else {
			b.AddWrite(target, 1) // count, not bytes → WrittenUnknownExt[ext]++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return UnknownLedger{}, err
	}
	rows.Close()

	// Fence tags from assistant text (segmentation cost; bounded).
	if err := s.foldVisibleAll(ctx, sinceDays, b); err != nil {
		return UnknownLedger{}, err
	}

	return UnknownLedger{
		Extensions:     b.WrittenUnknownExt,
		FenceTags:      b.Visible.ArtifactUnknownTags,
		TotalWrites:    total,
		ResolvedWrites: resolved,
	}, nil
}

// foldVisibleAll segments every assistant-text body in the window into b
// (used by the unknown-tag ledger).
func (s *Store) foldVisibleAll(ctx context.Context, sinceDays int, b *verbosity.Breakdown) error {
	where := ""
	var args []any
	if sinceDays > 0 {
		where = " AND timestamp >= ?"
		args = append(args, isoSinceDays(sinceDays))
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT raw_tool_output FROM actions
		  WHERE `+assistantTextFilter+`
		    AND raw_tool_output IS NOT NULL AND raw_tool_output != ''`+where,
		args...)
	if err != nil {
		return fmt.Errorf("store.LoadUnknownLedger: tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var body sql.NullString
		if err := rows.Scan(&body); err != nil {
			return fmt.Errorf("store.LoadUnknownLedger: tag scan: %w", err)
		}
		if body.Valid {
			b.AddVisibleText(body.String)
		}
	}
	return rows.Err()
}

// SessionTokenTotals reports the session's resolved model plus its summed
// output and reasoning tokens, for the verbosity card's est token/$ split (plan
// §7). Model prefers sessions.model and falls back to the most recent non-empty
// token_usage.model (the Cursor/CC sessions.model gap). Output and reasoning
// are summed separately because the cost engine bills reasoning at the output
// rate as a distinct slice (cost/engine.go) — the apportionment uses
// output_tokens only, reasoning is accounted on its own. Zero/empty when the
// session has no token rows; the surface then hides the $ honestly.
func (s *Store) SessionTokenTotals(ctx context.Context, sessionID string) (model string, output, reasoning int64, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(NULLIF(s.model, ''),
		           (SELECT tu.model FROM token_usage tu
		             WHERE tu.session_id = s.id AND COALESCE(tu.model, '') != ''
		             ORDER BY tu.id DESC LIMIT 1),
		           ''),
		  COALESCE((SELECT SUM(COALESCE(output_tokens, 0))    FROM token_usage WHERE session_id = s.id), 0),
		  COALESCE((SELECT SUM(COALESCE(reasoning_tokens, 0)) FROM token_usage WHERE session_id = s.id), 0)
		FROM sessions s WHERE s.id = ?`, sessionID,
	).Scan(&model, &output, &reasoning)
	if err != nil {
		return "", 0, 0, fmt.Errorf("store.SessionTokenTotals: %w", err)
	}
	return model, output, reasoning, nil
}

// VerbosityGroup is one row of an aggregate rollup: a dimension key (model
// name / project root / day) and its assembled Breakdown.
type VerbosityGroup struct {
	Key       string
	Breakdown *verbosity.Breakdown
}

// resolvedModelJoin resolves each action's session to a model, falling back to
// the session's most recent non-empty token_usage.model when sessions.model is
// blank (the Cursor/CC gap — ~80% of sessions carry no sessions.model but ~82%
// of those are recoverable from token_usage). Derived once per session, joined
// on action.session_id. Without this the by-model cut buckets most sessions as
// (unknown).
const resolvedModelJoin = ` JOIN (
		SELECT s2.id AS sid,
		       COALESCE(NULLIF(s2.model, ''),
		                (SELECT tu.model FROM token_usage tu
		                  WHERE tu.session_id = s2.id AND COALESCE(tu.model, '') != ''
		                  ORDER BY tu.id DESC LIMIT 1),
		                '(unknown)') AS rmodel
		  FROM sessions s2
	) rm ON rm.sid = a.session_id`

// verbosityKeyExpr maps an aggregate dimension to its SQL key expression and
// any extra JOIN it needs. Whitelist — the caller's dimension string never
// reaches SQL except through this table.
var verbosityKeyExpr = map[string]struct {
	expr string
	join string
}{
	"model":   {expr: "rm.rmodel", join: resolvedModelJoin},
	"day":     {expr: "substr(s.started_at, 1, 10)"},
	"project": {expr: "COALESCE(NULLIF(p.root_path, ''), '(unknown)')", join: " JOIN projects p ON a.project_id = p.id"},
}

// LoadVerbosityAggregate assembles a Breakdown per group across sessions
// started within the last sinceDays (<=0 = all history), grouped by
// dimension ∈ {model, day, project}. Two queries (visible + authored), folded
// in Go — the visible bodies are segmented at read time, so a very wide
// window does one SegmentVisible per assistant-text row (bounded, ~µs each).
func (s *Store) LoadVerbosityAggregate(ctx context.Context, dimension string, sinceDays int) ([]VerbosityGroup, error) {
	ke, ok := verbosityKeyExpr[dimension]
	if !ok {
		return nil, fmt.Errorf("store.LoadVerbosityAggregate: unknown dimension %q", dimension)
	}
	join := ke.join
	where := ""
	args := []any{}
	if sinceDays > 0 {
		where = " AND s.started_at >= ?"
		args = append(args, isoSinceDays(sinceDays))
	}

	groups := map[string]*verbosity.Breakdown{}
	getGroup := func(k string) *verbosity.Breakdown {
		b := groups[k]
		if b == nil {
			b = verbosity.NewBreakdown()
			groups[k] = b
		}
		return b
	}

	// Visible text per group.
	//nolint:gosec // G201: ke.expr/join are from the in-package whitelist above.
	visSQL := `SELECT ` + ke.expr + ` AS k, a.raw_tool_output
		   FROM actions a JOIN sessions s ON a.session_id = s.id` + join + `
		  WHERE ` + assistantTextFilter + `
		    AND a.raw_tool_output IS NOT NULL AND a.raw_tool_output != ''` + where
	if err := s.foldAggVisible(ctx, visSQL, args, getGroup); err != nil {
		return nil, err
	}

	// Authored code per group.
	//nolint:gosec // G201: ke.expr/join from whitelist; values are bound params.
	authSQL := `SELECT ` + ke.expr + ` AS k, a.action_type, COALESCE(a.raw_tool_name, ''), COALESCE(a.target, ''), a.content_bytes
		   FROM actions a JOIN sessions s ON a.session_id = s.id` + join + `
		  WHERE a.content_bytes IS NOT NULL AND a.content_bytes > 0
		    AND a.action_type IN (?, ?, ?)` + where
	authArgs := append([]any{models.ActionWriteFile, models.ActionEditFile, models.ActionRunCommand}, args...)
	if err := s.foldAggAuthored(ctx, authSQL, authArgs, getGroup); err != nil {
		return nil, err
	}

	out := make([]VerbosityGroup, 0, len(groups))
	for k, b := range groups {
		out = append(out, VerbosityGroup{Key: k, Breakdown: b})
	}
	return out, nil
}

// VerbosityGroupTokens is one (group, model) token total for the aggregate $
// estimate — a group (project/day) can span several models with different
// rates, so the handler prices each (key, model) row at that model's output
// rate and sums per key.
type VerbosityGroupTokens struct {
	Key       string
	Model     string
	Output    int64
	Reasoning int64
}

// verbosityTokenKeyExpr is the token-side key whitelist (joins token_usage, so
// tu.model resolves the model directly — no derived subquery needed). Mirrors
// verbosityKeyExpr's dimensions; the model key uses the same
// sessions.model→token_usage.model fallback so by-model groups align with the
// bytes aggregate.
var verbosityTokenKeyExpr = map[string]struct {
	expr string
	join string
}{
	"model":   {expr: "COALESCE(NULLIF(s.model, ''), NULLIF(tu.model, ''), '(unknown)')"},
	"day":     {expr: "substr(s.started_at, 1, 10)"},
	"project": {expr: "COALESCE(NULLIF(p.root_path, ''), '(unknown)')", join: " JOIN projects p ON s.project_id = p.id"},
}

// LoadVerbosityGroupTokens sums output and reasoning tokens per (group, model)
// across sessions started within sinceDays (<=0 = all), grouped by dimension ∈
// {model, day, project}. The handler prices each row at the model's output
// rate for the aggregate's est $ total. Unpriced models simply contribute no
// dollars (honest — never fabricated).
func (s *Store) LoadVerbosityGroupTokens(ctx context.Context, dimension string, sinceDays int) ([]VerbosityGroupTokens, error) {
	ke, ok := verbosityTokenKeyExpr[dimension]
	if !ok {
		return nil, fmt.Errorf("store.LoadVerbosityGroupTokens: unknown dimension %q", dimension)
	}
	where := ""
	args := []any{}
	if sinceDays > 0 {
		where = " AND s.started_at >= ?"
		args = append(args, isoSinceDays(sinceDays))
	}
	//nolint:gosec // G201: ke.expr/join from the in-package whitelist above; values are bound params.
	q := `SELECT ` + ke.expr + ` AS k,
	             COALESCE(NULLIF(s.model, ''), NULLIF(tu.model, ''), '(unknown)') AS m,
	             COALESCE(SUM(tu.output_tokens), 0), COALESCE(SUM(tu.reasoning_tokens), 0)
	        FROM token_usage tu JOIN sessions s ON tu.session_id = s.id` + ke.join + `
	       WHERE 1=1` + where + `
	       GROUP BY k, m`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.LoadVerbosityGroupTokens: %w", err)
	}
	defer rows.Close()
	var out []VerbosityGroupTokens
	for rows.Next() {
		var g VerbosityGroupTokens
		if err := rows.Scan(&g.Key, &g.Model, &g.Output, &g.Reasoning); err != nil {
			return nil, fmt.Errorf("store.LoadVerbosityGroupTokens: scan: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) foldAggVisible(ctx context.Context, query string, args []any, get func(string) *verbosity.Breakdown) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store.LoadVerbosityAggregate: visible: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var body sql.NullString
		if err := rows.Scan(&key, &body); err != nil {
			return fmt.Errorf("store.LoadVerbosityAggregate: visible scan: %w", err)
		}
		if body.Valid {
			get(key).AddVisibleText(body.String)
		}
	}
	return rows.Err()
}

func (s *Store) foldAggAuthored(ctx context.Context, query string, args []any, get func(string) *verbosity.Breakdown) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store.LoadVerbosityAggregate: authored: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, actionType, rawToolName, target string
		var bytes int64
		if err := rows.Scan(&key, &actionType, &rawToolName, &target, &bytes); err != nil {
			return fmt.Errorf("store.LoadVerbosityAggregate: authored scan: %w", err)
		}
		b := get(key)
		if actionType == models.ActionRunCommand {
			b.AddCommand(rawToolName, bytes)
		} else {
			b.AddWrite(target, bytes)
		}
	}
	return rows.Err()
}

// AuthoredCaptureStats reports how many of a session's authored-code actions
// (write/edit/run_command) carry a content_bytes measurement vs the total.
// It lets a surface distinguish "no code authored" (total == 0) from "code
// authored but not yet measured" (total > 0, captured == 0 — pre-migration-054
// rows, or a daemon not yet on the content_bytes build) so it can prompt an
// `observer backfill` instead of implying the session was prose-only.
func (s *Store) AuthoredCaptureStats(ctx context.Context, sessionID string) (captured, total int64, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN content_bytes IS NOT NULL AND content_bytes > 0 THEN 1 ELSE 0 END), 0),
		  COUNT(*)
		FROM actions
		WHERE session_id = ? AND action_type IN (?, ?, ?)`,
		sessionID, models.ActionWriteFile, models.ActionEditFile, models.ActionRunCommand,
	).Scan(&captured, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("store.AuthoredCaptureStats: %w", err)
	}
	return captured, total, nil
}

// foldAuthored attributes each file-write/edit and run_command action's
// content_bytes to a language (writes) or shell dialect (commands).
func (s *Store) foldAuthored(ctx context.Context, sessionID string, b *verbosity.Breakdown) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT action_type, COALESCE(raw_tool_name, ''), COALESCE(target, ''), content_bytes
		   FROM actions
		  WHERE session_id = ?
		    AND content_bytes IS NOT NULL AND content_bytes > 0
		    AND action_type IN (?, ?, ?)`,
		sessionID, models.ActionWriteFile, models.ActionEditFile, models.ActionRunCommand)
	if err != nil {
		return fmt.Errorf("store.LoadSessionVerbosity: authored: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var actionType, rawToolName, target string
		var bytes int64
		if err := rows.Scan(&actionType, &rawToolName, &target, &bytes); err != nil {
			return fmt.Errorf("store.LoadSessionVerbosity: authored scan: %w", err)
		}
		if actionType == models.ActionRunCommand {
			b.AddCommand(rawToolName, bytes)
		} else {
			b.AddWrite(target, bytes)
		}
	}
	return rows.Err()
}
