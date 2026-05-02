package scoring

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Scores is the per-session quality summary. A zero Scores is the correct
// value for a session with zero actions — callers shouldn't write it back.
type Scores struct {
	SessionID         string  `json:"session_id"`
	QualityScore      float64 `json:"quality_score"`
	RedundancyRatio   float64 `json:"redundancy_ratio"`
	ErrorRate         float64 `json:"error_rate"`
	ExplorationEff    float64 `json:"exploration_efficiency"`
	ContinuityScore   float64 `json:"continuity_score"`
	OnboardingCost    int64   `json:"onboarding_cost"`
	TurnsToFirstEdit  int     `json:"turns_to_first_edit"`
	RetryCostTokens   int64   `json:"retry_cost_tokens"`
	TotalActions      int     `json:"total_actions"`
	TotalFailures     int     `json:"total_failures"`
	StaleReads        int     `json:"stale_reads"`
	DistinctFilesRead int     `json:"distinct_files_read"`
	DistinctFilesEdit int     `json:"distinct_files_edited"`
}

// Scorer runs scoring passes against a DB.
type Scorer struct{ db *sql.DB }

// New wraps db.
func New(db *sql.DB) *Scorer { return &Scorer{db: db} }

// BatchOptions parameterizes BatchScore.
type BatchOptions struct {
	// OnlyUnscored, when true, skips sessions that already have a non-NULL
	// quality_score. Scoring is idempotent, so callers that want to refresh
	// after a freshness-pipeline improvement should set this false.
	OnlyUnscored bool
	// IdleAtLeast, when non-zero, skips sessions whose latest action is
	// within this duration of now — these sessions are still "live".
	IdleAtLeast time.Duration
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// BatchResult summarizes a batch pass.
type BatchResult struct {
	Considered int
	Scored     int
	Skipped    int
	Errors     int
	DurationMs int64
}

// BatchScore walks the sessions table and scores each session that matches
// opts. Errors on a single session are counted but do not abort the pass.
func (s *Scorer) BatchScore(ctx context.Context, opts BatchOptions) (BatchResult, error) {
	start := time.Now()
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}

	q := `SELECT id FROM sessions`
	var where []string
	if opts.OnlyUnscored {
		where = append(where, "quality_score IS NULL")
	}
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return BatchResult{}, fmt.Errorf("scoring: list sessions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return BatchResult{}, fmt.Errorf("scoring: scan session id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	res := BatchResult{Considered: len(ids)}
	cutoff := opts.Now().Add(-opts.IdleAtLeast)

	for _, id := range ids {
		if opts.IdleAtLeast > 0 {
			idle, err := s.sessionIdleSince(ctx, id, cutoff)
			if err != nil {
				res.Errors++
				continue
			}
			if !idle {
				res.Skipped++
				continue
			}
		}
		scores, err := s.ScoreSession(ctx, id)
		if err != nil {
			res.Errors++
			continue
		}
		if scores.TotalActions == 0 {
			res.Skipped++
			continue
		}
		if err := s.Write(ctx, scores); err != nil {
			res.Errors++
			continue
		}
		res.Scored++
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// ScoreSession computes Scores for a single session. It does not write
// anything; callers use Write separately so tests can inspect the intermediate
// result.
func (s *Scorer) ScoreSession(ctx context.Context, sessionID string) (Scores, error) {
	out := Scores{SessionID: sessionID, TurnsToFirstEdit: -1}

	actions, err := s.loadActions(ctx, sessionID)
	if err != nil {
		return Scores{}, err
	}
	out.TotalActions = len(actions)
	if len(actions) == 0 {
		return out, nil
	}

	readsByTarget := map[string]bool{}
	editsByTarget := map[string]bool{}
	var readOrRunCount int
	var firstEditIdx int = -1
	var lastFailureIdx = -1

	for i, a := range actions {
		if !a.Success {
			out.TotalFailures++
			lastFailureIdx = i
		}
		switch a.ActionType {
		case models.ActionReadFile:
			readOrRunCount++
			if a.Target != "" {
				readsByTarget[a.Target] = true
			}
			if a.Freshness == models.FreshnessStale {
				out.StaleReads++
			}
		case models.ActionEditFile, models.ActionWriteFile:
			if firstEditIdx < 0 {
				firstEditIdx = i
				out.TurnsToFirstEdit = a.TurnIndex
			}
			if a.Target != "" {
				editsByTarget[a.Target] = true
			}
		case models.ActionRunCommand:
			readOrRunCount++
		}
	}

	if readOrRunCount > 0 {
		out.RedundancyRatio = float64(out.StaleReads) / float64(readOrRunCount)
	}
	out.ErrorRate = float64(out.TotalFailures) / float64(out.TotalActions)

	out.DistinctFilesRead = len(readsByTarget)
	out.DistinctFilesEdit = len(editsByTarget)
	touched := len(readsByTarget)
	for k := range editsByTarget {
		if !readsByTarget[k] {
			touched++
		}
	}
	if touched > 0 {
		out.ExplorationEff = clamp01(float64(out.DistinctFilesEdit) / float64(touched))
	}

	// Continuity: sessions that run long enough to build working memory
	// score higher. A soft sigmoid on TotalActions feels about right — a
	// 1-action session is noise; a 50-action session has flow.
	out.ContinuityScore = continuityFrom(out.TotalActions)

	// Tokens-side metrics, best-effort.
	if firstEditIdx >= 0 {
		out.OnboardingCost, _ = s.sumTokensBefore(ctx, sessionID, actions[firstEditIdx].Timestamp)
	} else {
		// No edits means the whole session was onboarding — cost is total.
		out.OnboardingCost, _ = s.sumTokensBefore(ctx, sessionID, time.Time{})
	}
	if lastFailureIdx >= 0 {
		out.RetryCostTokens, _ = s.sumTokensAfterFailures(ctx, sessionID)
	}

	out.QualityScore = 0.4*(1-out.RedundancyRatio) +
		0.3*(1-out.ErrorRate) +
		0.2*out.ExplorationEff +
		0.1*out.ContinuityScore
	return out, nil
}

// Write persists scores back into the sessions row.
func (s *Scorer) Write(ctx context.Context, scores Scores) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET
		quality_score = ?, redundancy_ratio = ?, error_rate = ?,
		onboarding_cost = ?, turns_to_first_edit = ?, retry_cost_tokens = ?
		WHERE id = ?`,
		scores.QualityScore, scores.RedundancyRatio, scores.ErrorRate,
		scores.OnboardingCost, nullableInt(scores.TurnsToFirstEdit),
		scores.RetryCostTokens, scores.SessionID)
	if err != nil {
		return fmt.Errorf("scoring: write %s: %w", scores.SessionID, err)
	}
	return nil
}

// loadActions pulls the ordered action rows the scorer needs. Minimal
// columns to keep memory flat for long sessions.
func (s *Scorer) loadActions(ctx context.Context, sessionID string) ([]models.Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, timestamp, turn_index, action_type, target, success,
		        COALESCE(freshness, ''), COALESCE(change_detected, 0)
		 FROM actions WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("scoring: query actions: %w", err)
	}
	defer rows.Close()
	var out []models.Action
	for rows.Next() {
		var a models.Action
		var ts string
		var successInt, changeInt int
		if err := rows.Scan(&a.ID, &ts, &a.TurnIndex, &a.ActionType, &a.Target,
			&successInt, &a.Freshness, &changeInt); err != nil {
			return nil, fmt.Errorf("scoring: scan action: %w", err)
		}
		a.Success = successInt != 0
		a.ChangeDetected = changeInt != 0
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			a.Timestamp = parsed
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scoring: actions rows: %w", err)
	}
	return out, nil
}

// sumTokensBefore returns input+output tokens for token_usage rows in the
// session with timestamp < cutoff. A zero cutoff means "all rows".
func (s *Scorer) sumTokensBefore(ctx context.Context, sessionID string, cutoff time.Time) (int64, error) {
	q := `SELECT COALESCE(SUM(input_tokens), 0) + COALESCE(SUM(output_tokens), 0)
	      FROM token_usage WHERE session_id = ?`
	args := []any{sessionID}
	if !cutoff.IsZero() {
		q += ` AND timestamp < ?`
		args = append(args, cutoff.UTC().Format(time.RFC3339Nano))
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("scoring: sum tokens: %w", err)
	}
	return total, nil
}

// sumTokensAfterFailures returns input+output tokens for turns that happen
// after a failed action but within the same session. Proxy: we bucket all
// failures together and sum tokens between each failure and the next
// success. This over-estimates when the failure had nothing to do with the
// later turns, but is the simplest first-cut "retry budget" proxy.
func (s *Scorer) sumTokensAfterFailures(ctx context.Context, sessionID string) (int64, error) {
	// Find all failure timestamps for this session.
	rows, err := s.db.QueryContext(ctx,
		`SELECT timestamp FROM actions
		 WHERE session_id = ? AND success = 0 ORDER BY timestamp`, sessionID)
	if err != nil {
		return 0, fmt.Errorf("scoring: failure timestamps: %w", err)
	}
	defer rows.Close()
	var times []time.Time
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return 0, fmt.Errorf("scoring: scan failure ts: %w", err)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			times = append(times, parsed)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(times) == 0 {
		return 0, nil
	}
	// Sum token_usage rows that occur within 5 minutes after each failure.
	// Overlapping windows are OK — we only want a rough "cost of retries".
	// Using a CTE would be cleaner, but with <1k failures per session the
	// per-failure query is fine.
	var total int64
	for _, t := range times {
		var n int64
		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(input_tokens), 0) + COALESCE(SUM(output_tokens), 0)
			 FROM token_usage WHERE session_id = ?
			   AND timestamp >= ? AND timestamp < ?`,
			sessionID,
			t.UTC().Format(time.RFC3339Nano),
			t.Add(5*time.Minute).UTC().Format(time.RFC3339Nano)).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("scoring: retry token sum: %w", err)
		}
		total += n
	}
	return total, nil
}

// sessionIdleSince reports whether the session's latest action is older than
// cutoff (meaning the session is idle and safe to score).
func (s *Scorer) sessionIdleSince(ctx context.Context, sessionID string, cutoff time.Time) (bool, error) {
	var ts sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(timestamp) FROM actions WHERE session_id = ?`, sessionID).Scan(&ts)
	if err != nil {
		return false, fmt.Errorf("scoring: idle check: %w", err)
	}
	if !ts.Valid {
		// No actions → treat as idle; caller's score path will skip it
		// anyway since TotalActions will be 0.
		return true, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts.String)
	if err != nil {
		return false, fmt.Errorf("scoring: parse timestamp %q: %w", ts.String, err)
	}
	return parsed.Before(cutoff), nil
}

// continuityFrom maps TotalActions to a [0,1] continuity score. Chosen so a
// 50-action session scores ~0.88 and a 5-action session scores ~0.39.
func continuityFrom(totalActions int) float64 {
	if totalActions <= 0 {
		return 0
	}
	// 1 - exp(-n/20) gives a smooth ramp from 0 to 1, halfway around n=14.
	return clamp01(1 - math.Exp(-float64(totalActions)/20.0))
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func nullableInt(n int) sql.NullInt64 {
	if n < 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}
