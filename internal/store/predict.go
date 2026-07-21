package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/predict"
)

// PredictShape is the session's assembled cost-estimate substrate — the
// raw token shape the Next-Message Cost Predictor scores. Rates are NOT
// resolved here (the dashboard/CLI caller injects them via
// cost.Table.Lookup) so this seam stays cost-package-free, mirroring the
// cachetrack-forecast split. Per the §0 findings the substrate is
// token_usage, not api_turns.
type PredictShape struct {
	Tool             string
	Model            string
	ProjectID        int64
	PrefixTokens     int64
	TurnSamples      []predict.TurnSample
	TurnsPerMessage  []int
	ObservedMessages int
}

// LoadSessionShape assembles the predictor's per-session input from
// token_usage (+ user_prompt action boundaries for the turns-per-message
// fan-out). Returns sql.ErrNoRows when the session doesn't exist.
//
//   - Model: sessions.model, falling back to the dominant token_usage.model
//     (the same fallback handleSessionDetail uses, since sessions.model is
//     empty for ~89% of claude-code sessions).
//   - PrefixTokens (P "now"): the most-recent turn's cache_read_tokens —
//     the running cache prefix re-read every turn. 0 for an uncached
//     provider.
//   - TurnSamples: per-turn (fresh-input, output) over the session's token
//     rows. Fresh = max(input − cache_read, 0).
//   - TurnsPerMessage / ObservedMessages: agent turns bucketed between
//     consecutive user_prompt timestamps (only messages with ≥1 captured
//     turn). Empty when the session has no user_prompt actions.
func (s *Store) LoadSessionShape(ctx context.Context, sessionID string) (PredictShape, error) {
	var shape PredictShape

	var model sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT tool, COALESCE(model, ''), project_id FROM sessions WHERE id = ?`, sessionID).
		Scan(&shape.Tool, &model, &shape.ProjectID)
	if err != nil {
		return shape, err
	}
	shape.Model = model.String

	// Model fallback: dominant token_usage.model by token volume.
	if shape.Model == "" {
		var fallback sql.NullString
		ferr := s.db.QueryRowContext(ctx, `
			SELECT model FROM token_usage
			 WHERE session_id = ? AND model IS NOT NULL AND model <> ''
			 GROUP BY model
			 ORDER BY SUM(COALESCE(input_tokens,0)+COALESCE(output_tokens,0)) DESC
			 LIMIT 1`, sessionID).Scan(&fallback)
		if ferr != nil && !errors.Is(ferr, sql.ErrNoRows) {
			return shape, fmt.Errorf("model fallback: %w", ferr)
		}
		shape.Model = fallback.String
	}

	// P "now" — latest non-zero cache_read prefix.
	var prefix sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT cache_read_tokens FROM token_usage
		 WHERE session_id = ? AND COALESCE(cache_read_tokens,0) > 0
		 ORDER BY timestamp DESC, id DESC LIMIT 1`, sessionID).Scan(&prefix); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return shape, fmt.Errorf("prefix tokens: %w", err)
		}
	}
	shape.PrefixTokens = prefix.Int64

	// Per-turn (fresh-input, output) samples + the turn timestamps used
	// for the user-message bucketing.
	turnTimes, samples, err := loadTurnSamples(ctx, s.db, sessionID)
	if err != nil {
		return shape, err
	}
	shape.TurnSamples = samples

	// user_prompt boundaries → turns-per-message fan-out.
	promptTimes, err := loadUserPromptTimes(ctx, s.db, sessionID)
	if err != nil {
		return shape, err
	}
	shape.TurnsPerMessage = bucketTurnsPerMessage(promptTimes, turnTimes)
	shape.ObservedMessages = len(shape.TurnsPerMessage)

	return shape, nil
}

// loadTurnSamples reads the session's token rows in time order, returning
// the parsed turn timestamps (for bucketing) and the (fresh-input,
// output) samples. Rows with no input and no output are skipped.
func loadTurnSamples(ctx context.Context, db *sql.DB, sessionID string) ([]time.Time, []predict.TurnSample, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT timestamp,
		       COALESCE(input_tokens,0),
		       COALESCE(output_tokens,0),
		       COALESCE(cache_read_tokens,0)
		  FROM token_usage
		 WHERE session_id = ?
		 ORDER BY timestamp ASC, id ASC`, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("turn samples: %w", err)
	}
	defer rows.Close()

	var times []time.Time
	var samples []predict.TurnSample
	for rows.Next() {
		var ts string
		var in, out, cacheRead int64
		if err := rows.Scan(&ts, &in, &out, &cacheRead); err != nil {
			return nil, nil, fmt.Errorf("scan turn sample: %w", err)
		}
		if in == 0 && out == 0 {
			continue
		}
		fresh := in - cacheRead
		if fresh < 0 {
			fresh = 0
		}
		samples = append(samples, predict.TurnSample{FreshInput: fresh, Output: out})
		if t, ok := parseDBTime(ts); ok {
			times = append(times, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("turn sample rows: %w", err)
	}
	return times, samples, nil
}

// loadUserPromptTimes returns the session's user_prompt action timestamps
// in ascending order — the user-message boundaries.
func loadUserPromptTimes(ctx context.Context, db *sql.DB, sessionID string) ([]time.Time, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT timestamp FROM actions
		 WHERE session_id = ? AND action_type = 'user_prompt'
		 ORDER BY timestamp ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("user_prompt times: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("scan user_prompt: %w", err)
		}
		if t, ok := parseDBTime(ts); ok {
			out = append(out, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("user_prompt rows: %w", err)
	}
	return out, nil
}

// bucketTurnsPerMessage counts turn timestamps falling into each
// [prompt[i], prompt[i+1]) interval (the last interval runs to +∞).
// Only intervals with ≥1 turn are returned, so the result is the
// observed fan-out distribution (a prompt with no captured turns is
// noise, not a zero-cost message). Empty prompts → empty result.
func bucketTurnsPerMessage(prompts, turns []time.Time) []int {
	if len(prompts) == 0 || len(turns) == 0 {
		return nil
	}
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].Before(prompts[j]) })
	sort.Slice(turns, func(i, j int) bool { return turns[i].Before(turns[j]) })

	counts := make([]int, len(prompts))
	ti := 0
	// Skip turns before the first prompt (pre-session noise / system).
	for ti < len(turns) && turns[ti].Before(prompts[0]) {
		ti++
	}
	for i := 0; i < len(prompts); i++ {
		var end time.Time
		hasEnd := i+1 < len(prompts)
		if hasEnd {
			end = prompts[i+1]
		}
		for ti < len(turns) && (!hasEnd || turns[ti].Before(end)) {
			counts[i]++
			ti++
		}
	}
	out := make([]int, 0, len(counts))
	for _, c := range counts {
		if c >= 1 {
			out = append(out, c)
		}
	}
	return out
}

// LoadToolProjectPrior returns the cross-session turns-per-message prior
// for (tool, projectID) — the tier-2 fallback when the current session
// has no user_prompt boundaries. Each sample is one comparable session's
// average turns-per-user-message (turn rows ÷ user_prompt count), a cheap
// approximation that avoids per-session bucketing across the corpus.
//
// Scopes to the project first; widens to the tool when the project yields
// fewer than 3 comparable sessions. windowDays bounds recency (0 = no
// bound). Returns nil when no comparable session carries user_prompt
// boundaries (caller then falls to the static default).
func (s *Store) LoadToolProjectPrior(ctx context.Context, tool string, projectID int64, windowDays int) ([]int, error) {
	prior, err := s.loadPriorScoped(ctx, tool, projectID, windowDays)
	if err != nil {
		return nil, err
	}
	if len(prior) < minPriorSessions {
		// Widen to tool-wide.
		wide, werr := s.loadPriorScoped(ctx, tool, 0, windowDays)
		if werr != nil {
			return nil, werr
		}
		if len(wide) > len(prior) {
			prior = wide
		}
	}
	// Min-sample guard: a prior built from one or two sessions yields
	// meaningless quantiles (and, on a retention-pruned DB where most
	// sessions have lost their user_prompt boundaries, an inflated
	// token_turns/user_prompt ratio → a flat, wrong band). Below the
	// floor, return nil so the estimator falls to its static default
	// tier rather than labelling a 1-session guess as a "prior".
	if len(prior) < minPriorSessions {
		return nil, nil
	}
	return prior, nil
}

// minPriorSessions is the floor for trusting the cross-session T prior.
// Below it the quantiles aren't meaningful; the estimator uses the
// static default fan-out instead.
const minPriorSessions = 3

func (s *Store) loadPriorScoped(ctx context.Context, tool string, projectID int64, windowDays int) ([]int, error) {
	args := []any{tool}
	q := `
		SELECT CAST(ROUND(
		         (SELECT COUNT(*) FROM token_usage k WHERE k.session_id = s.id) * 1.0 /
		         (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id AND a.action_type = 'user_prompt')
		       ) AS INTEGER) AS avg_t
		  FROM sessions s
		 WHERE s.tool = ?
		   AND EXISTS (SELECT 1 FROM actions a WHERE a.session_id = s.id AND a.action_type = 'user_prompt')
		   AND EXISTS (SELECT 1 FROM token_usage k WHERE k.session_id = s.id)`
	if projectID > 0 {
		q += ` AND s.project_id = ?`
		args = append(args, projectID)
	}
	if windowDays > 0 {
		q += ` AND s.started_at >= datetime('now', ?)`
		args = append(args, fmt.Sprintf("-%d days", windowDays))
	}
	q += ` ORDER BY s.started_at DESC LIMIT 100`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("prior scoped: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var t sql.NullInt64
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan prior: %w", err)
		}
		if t.Valid && t.Int64 >= 1 {
			out = append(out, int(t.Int64))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prior rows: %w", err)
	}
	return out, nil
}

// InsertLimitSnapshot persists one rate-limit observation (migration 049,
// NODE-LOCAL). The only writer of limit_snapshots — per the one-owner
// rule. Nil optional fields become NULL columns.
func (s *Store) InsertLimitSnapshot(ctx context.Context, snap models.LimitSnapshot) error {
	_, err := s.db.ExecContext(
		ctx, `
		INSERT INTO limit_snapshots
		  (scope_hash, provider, session_id, observed_at,
		   window_5h_util, window_5h_reset, window_7d_util, window_7d_reset,
		   req_limit, req_remaining, req_reset, tok_limit, tok_remaining, tok_reset,
		   status, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.ScopeHash, snap.Provider, nullStr(snap.SessionID), snap.ObservedAt.Unix(),
		nullF(snap.Window5hUtil), nullI(snap.Window5hReset), nullF(snap.Window7dUtil), nullI(snap.Window7dReset),
		nullI(snap.ReqLimit), nullI(snap.ReqRemaining), nullI(snap.ReqReset),
		nullI(snap.TokLimit), nullI(snap.TokRemaining), nullI(snap.TokReset),
		nullStr(snap.Status), nullStr(snap.Raw),
	)
	if err != nil {
		return fmt.Errorf("store.InsertLimitSnapshot: %w", err)
	}
	return nil
}

// LatestLimitSnapshot returns the most-recent snapshot for (scopeHash,
// provider), or ok=false when none exists. Indexed by
// idx_limit_snapshots_scope.
func (s *Store) LatestLimitSnapshot(ctx context.Context, scopeHash, provider string) (models.LimitSnapshot, bool, error) {
	var snap models.LimitSnapshot
	var observedUnix int64
	var w5u, w7u sql.NullFloat64
	var w5r, w7r, rl, rr, rrst, tl, tr, trst sql.NullInt64
	var sid, status, raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, scope_hash, provider, session_id, observed_at,
		       window_5h_util, window_5h_reset, window_7d_util, window_7d_reset,
		       req_limit, req_remaining, req_reset, tok_limit, tok_remaining, tok_reset,
		       status, raw
		  FROM limit_snapshots
		 WHERE scope_hash = ? AND provider = ?
		 ORDER BY observed_at DESC, id DESC LIMIT 1`, scopeHash, provider).
		Scan(&snap.ID, &snap.ScopeHash, &snap.Provider, &sid, &observedUnix,
			&w5u, &w5r, &w7u, &w7r, &rl, &rr, &rrst, &tl, &tr, &trst, &status, &raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return snap, false, nil
		}
		return snap, false, fmt.Errorf("store.LatestLimitSnapshot: %w", err)
	}
	snap.ObservedAt = time.Unix(observedUnix, 0).UTC()
	snap.SessionID = sid.String
	snap.Status = status.String
	snap.Raw = raw.String
	snap.Window5hUtil = fptr(w5u)
	snap.Window7dUtil = fptr(w7u)
	snap.Window5hReset = iptr(w5r)
	snap.Window7dReset = iptr(w7r)
	snap.ReqLimit, snap.ReqRemaining, snap.ReqReset = iptr(rl), iptr(rr), iptr(rrst)
	snap.TokLimit, snap.TokRemaining, snap.TokReset = iptr(tl), iptr(tr), iptr(trst)
	return snap, true, nil
}

// LatestLimitSnapshotForTool returns the most-recent snapshot for
// `provider` whose source session belongs to `tool`, or ok=false when
// that tool has never observed a window. This attributes the gauge to
// the credential that actually produced it: the unified 5h/weekly
// subscription windows come only from a tool whose proxied traffic
// carried those headers (Claude Code's subscription OAuth), so a tool
// like cline-cli — which routes a different credential and emits none —
// no longer inherits another tool's window. Distinct from the raw
// scope+provider LatestLimitSnapshot read; the deeper per-credential
// scope_hash derivation stays the R4 follow-up. Snapshots with no
// session_id (early stragglers) don't join and are correctly skipped.
func (s *Store) LatestLimitSnapshotForTool(ctx context.Context, provider, tool string) (models.LimitSnapshot, bool, error) {
	var snap models.LimitSnapshot
	var observedUnix int64
	var w5u, w7u sql.NullFloat64
	var w5r, w7r, rl, rr, rrst, tl, tr, trst sql.NullInt64
	var sid, status, raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT l.id, l.scope_hash, l.provider, l.session_id, l.observed_at,
		       l.window_5h_util, l.window_5h_reset, l.window_7d_util, l.window_7d_reset,
		       l.req_limit, l.req_remaining, l.req_reset, l.tok_limit, l.tok_remaining, l.tok_reset,
		       l.status, l.raw
		  FROM limit_snapshots l
		  JOIN sessions s ON s.id = l.session_id
		 WHERE l.provider = ? AND s.tool = ?
		 ORDER BY l.observed_at DESC, l.id DESC LIMIT 1`, provider, tool).
		Scan(&snap.ID, &snap.ScopeHash, &snap.Provider, &sid, &observedUnix,
			&w5u, &w5r, &w7u, &w7r, &rl, &rr, &rrst, &tl, &tr, &trst, &status, &raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return snap, false, nil
		}
		return snap, false, fmt.Errorf("store.LatestLimitSnapshotForTool: %w", err)
	}
	snap.ObservedAt = time.Unix(observedUnix, 0).UTC()
	snap.SessionID = sid.String
	snap.Status = status.String
	snap.Raw = raw.String
	snap.Window5hUtil = fptr(w5u)
	snap.Window7dUtil = fptr(w7u)
	snap.Window5hReset = iptr(w5r)
	snap.Window7dReset = iptr(w7r)
	snap.ReqLimit, snap.ReqRemaining, snap.ReqReset = iptr(rl), iptr(rr), iptr(rrst)
	snap.TokLimit, snap.TokRemaining, snap.TokReset = iptr(tl), iptr(tr), iptr(trst)
	return snap, true, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullI(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func fptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func iptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// parseDBTime parses the RFC3339(Nano) timestamps the store writes.
// Returns ok=false on an unparseable value so the caller can skip it.
func parseDBTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}
