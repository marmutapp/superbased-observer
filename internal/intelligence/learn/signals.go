package learn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SignalType categorises one row of retrieval_signals (K43 — Tier 3).
type SignalType string

const (
	// SignalRetrieveStashed records a successful retrieve_stashed MCP
	// call. action_id is unset; payload carries the sha.
	SignalRetrieveStashed SignalType = "retrieve_stashed"
	// SignalSearchHit records a non-empty search_past_outputs result
	// row. action_id is the FTS5 hit's actions.id; payload carries
	// the originating search query.
	SignalSearchHit SignalType = "search_hit"
)

// SignalRecorder appends rows to the retrieval_signals table — the
// load-bearing input to the K43 self-learning feedback loop. Every
// time the model retrieves stashed content or follows up on a search
// hit, the recorder logs one row; the [PatternMiner] later derives
// "high-retrieval-rate" signals from the aggregate.
//
// All write methods are best-effort and return nil on benign DB errors
// (e.g. closed DB during shutdown) — the loss of a single signal row
// must never fail an MCP tool call. Errors that signal real bugs (bad
// SQL, type mismatches) propagate so tests can catch them.
type SignalRecorder struct{ db *sql.DB }

// NewSignalRecorder wraps db.
func NewSignalRecorder(db *sql.DB) *SignalRecorder {
	return &SignalRecorder{db: db}
}

// RecordRetrieveStashed appends a "retrieve_stashed" signal. sha is
// the 64-char hex content hash. sessionID may be empty (not all
// callers thread it).
func (r *SignalRecorder) RecordRetrieveStashed(ctx context.Context, sha, sessionID string) error {
	if r == nil || r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, session_id, payload)
		 VALUES (NULL, ?, ?, ?, ?)`,
		string(SignalRetrieveStashed), time.Now().UTC().Format(time.RFC3339Nano),
		nullIfEmpty(sessionID), sha)
	if err != nil {
		return fmt.Errorf("learn.SignalRecorder.RecordRetrieveStashed: %w", err)
	}
	return nil
}

// RecordSearchHit appends a "search_hit" signal for a single FTS5 row.
// query is the originating search expression; sessionID may be empty.
func (r *SignalRecorder) RecordSearchHit(ctx context.Context, actionID int64, query, sessionID string) error {
	if r == nil || r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO retrieval_signals (action_id, signal_type, signal_at, session_id, payload)
		 VALUES (?, ?, ?, ?, ?)`,
		actionID, string(SignalSearchHit),
		time.Now().UTC().Format(time.RFC3339Nano),
		nullIfEmpty(sessionID), query)
	if err != nil {
		return fmt.Errorf("learn.SignalRecorder.RecordSearchHit: %w", err)
	}
	return nil
}

// nullIfEmpty returns sql.NullString{Valid:false} for empty strings so
// the column lands NULL rather than ”. Lets `WHERE session_id IS NOT NULL`
// be a meaningful filter on the report side.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RetrievalRateReport is the aggregate the [SignalRecorder] surfaces
// to callers asking "is the compression layer actually helping?"
type RetrievalRateReport struct {
	// SinceDays is the lookback window the report covers.
	SinceDays int `json:"since_days"`
	// StashRetrievals is the count of "retrieve_stashed" signals in
	// the window.
	StashRetrievals int `json:"stash_retrievals"`
	// SearchHits is the count of "search_hit" signals in the window.
	SearchHits int `json:"search_hits"`
	// TotalStashes is the count of stash compression_events rows in
	// the window — the denominator of [RetrieveRate].
	TotalStashes int `json:"total_stashes"`
	// RetrieveRate is StashRetrievals / TotalStashes, or 0 when
	// TotalStashes is 0. Can exceed 1.0 when the model retrieves the
	// same sha more than once (good signal: load-bearing content).
	RetrieveRate float64 `json:"retrieve_rate"`
	// TopRetrievedShas surfaces shas with the highest retrieve count
	// — these are good candidates for an inline-rather-than-stash
	// override (model keeps asking for them anyway).
	TopRetrievedShas []ShaCount `json:"top_retrieved_shas,omitempty"`
	// TopSearchedActions surfaces action_ids with the highest hit
	// count — these are load-bearing tool outputs the model returns
	// to repeatedly across sessions.
	TopSearchedActions []ActionCount `json:"top_searched_actions,omitempty"`
	// Hints surfaces threshold-tuning recommendations derived from
	// the aggregate. Always non-nil (may be empty when no
	// recommendation applies); JSON omits the field when empty for
	// dashboard zero-state.
	Hints []ThresholdHint `json:"hints,omitempty"`
}

// ShaCount is one row of a top-shas list.
type ShaCount struct {
	Sha   string `json:"sha"`
	Count int    `json:"count"`
}

// ActionCount is one row of a top-action_ids list.
type ActionCount struct {
	ActionID int64 `json:"action_id"`
	Count    int   `json:"count"`
}

// ThresholdHint is one tuning recommendation derived from the
// aggregate retrieval-rate data. Surface in the dashboard so users
// can act on dogfood signal without staring at SQL output.
//
// Severity reads as: "info" → just FYI; "consider" → likely
// worth tuning; the UI can colour-code if it likes.
type ThresholdHint struct {
	// Kind is the hint family: "stash-retrieve-rate" today; future
	// hints (e.g. "stash-size-distribution", "shape-bucket") slot in
	// here without breaking consumers.
	Kind string `json:"kind"`
	// Severity is "info" | "consider".
	Severity string `json:"severity"`
	// Message is a one-sentence human-readable recommendation.
	Message string `json:"message"`
	// Metric is the numeric anchor for the recommendation
	// (e.g. retrieve_rate when Kind = "stash-retrieve-rate"). Lets
	// the UI display "based on X% retrieve rate" without re-deriving.
	Metric float64 `json:"metric"`
}

// PatternMiner aggregates retrieval_signals into the K43 report shape.
// Reads are time-windowed; the typical caller is the cost / dashboard
// layer asking for the last week.
type PatternMiner struct{ db *sql.DB }

// NewPatternMiner wraps db.
func NewPatternMiner(db *sql.DB) *PatternMiner {
	return &PatternMiner{db: db}
}

// ReportOptions parameterizes Report. Tool/Project are optional;
// empty means "no filter".
type ReportOptions struct {
	Days int
	// Tool restricts to retrieval_signals / compression_events rows
	// whose backing action / api_turn / session was for this tool.
	// Empty → no tool filter.
	Tool string
	// Project restricts to rows whose backing action / api_turn /
	// session was for this project (project root_path). Empty → no
	// project filter.
	Project string
}

// Report builds the K43 retrieval-rate aggregate over the last
// opts.Days days. Default 7 when opts.Days <= 0; cap 90. Tool +
// project filters apply across all sub-queries by joining through
// actions / api_turns / sessions to projects.
func (m *PatternMiner) Report(ctx context.Context, opts ReportOptions) (RetrievalRateReport, error) {
	days := opts.Days
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	out := RetrievalRateReport{SinceDays: days}

	// retrieval_signals filter — joins through action_id → actions →
	// projects when a tool/project filter is set. All live signals in
	// the v1.6.3 corpus have action_id populated (session_id is
	// nullable and historically left empty), so action-based joins are
	// the load-bearing path.
	rsJoin, rsWhere, rsArgs := signalsFilter(opts.Tool, opts.Project)

	if err := m.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM retrieval_signals rs`+rsJoin+
			` WHERE rs.signal_type = ? AND rs.signal_at >= ?`+rsWhere,
		append([]any{string(SignalRetrieveStashed), cutoff}, rsArgs...)...,
	).Scan(&out.StashRetrievals); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("learn.PatternMiner.Report: count stashes: %w", err)
		}
	}
	if err := m.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM retrieval_signals rs`+rsJoin+
			` WHERE rs.signal_type = ? AND rs.signal_at >= ?`+rsWhere,
		append([]any{string(SignalSearchHit), cutoff}, rsArgs...)...,
	).Scan(&out.SearchHits); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("learn.PatternMiner.Report: count search hits: %w", err)
		}
	}
	// Total stashes — denominator of RetrieveRate. compression_events
	// rows with mechanism = 'stash' are the canonical record. We
	// gracefully no-op if the events table doesn't exist (e.g. fresh
	// install before any compression has fired) so the K43 surface
	// works on day-zero installs without erroring. Tool/project join
	// goes through api_turns → sessions → projects.
	ceJoin, ceWhere, ceArgs := compressionEventsFilter(opts.Tool, opts.Project)
	if err := m.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM compression_events ce`+ceJoin+
			` WHERE ce.mechanism = 'stash' AND ce.timestamp >= ?`+ceWhere,
		append([]any{cutoff}, ceArgs...)...,
	).Scan(&out.TotalStashes); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			// compression_events missing → leave TotalStashes = 0
			// rather than failing the whole report.
			out.TotalStashes = 0
		}
	}
	if out.TotalStashes > 0 {
		out.RetrieveRate = float64(out.StashRetrievals) / float64(out.TotalStashes)
	}

	//nolint:gosec // G202: SQL structure and rsJoin/rsWhere are code-constant scope fragments; all values are bound via ? args.
	rows, err := m.db.QueryContext(ctx,
		`SELECT rs.payload, COUNT(*) FROM retrieval_signals rs`+rsJoin+
			` WHERE rs.signal_type = ? AND rs.signal_at >= ? AND rs.payload != ''`+rsWhere+`
		 GROUP BY rs.payload ORDER BY COUNT(*) DESC LIMIT 10`,
		append([]any{string(SignalRetrieveStashed), cutoff}, rsArgs...)...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var sc ShaCount
			if err := rows.Scan(&sc.Sha, &sc.Count); err != nil {
				return out, fmt.Errorf("learn.PatternMiner.Report: scan sha: %w", err)
			}
			out.TopRetrievedShas = append(out.TopRetrievedShas, sc)
		}
	}

	//nolint:gosec // G202: SQL structure and rsJoin/rsWhere are code-constant scope fragments; all values are bound via ? args.
	arows, err := m.db.QueryContext(ctx,
		`SELECT rs.action_id, COUNT(*) FROM retrieval_signals rs`+rsJoin+
			` WHERE rs.signal_type = ? AND rs.signal_at >= ? AND rs.action_id IS NOT NULL`+rsWhere+`
		 GROUP BY rs.action_id ORDER BY COUNT(*) DESC LIMIT 10`,
		append([]any{string(SignalSearchHit), cutoff}, rsArgs...)...)
	if err == nil {
		defer arows.Close()
		for arows.Next() {
			var ac ActionCount
			if err := arows.Scan(&ac.ActionID, &ac.Count); err != nil {
				return out, fmt.Errorf("learn.PatternMiner.Report: scan action: %w", err)
			}
			out.TopSearchedActions = append(out.TopSearchedActions, ac)
		}
	}

	out.Hints = deriveThresholdHints(out)
	return out, nil
}

// signalsFilter builds the optional JOIN + WHERE clauses for filtering
// retrieval_signals by tool/project, going through action_id →
// actions → projects. Returns ("", "", nil) when both filters are empty.
func signalsFilter(tool, project string) (joinSQL, whereSQL string, args []any) {
	if tool == "" && project == "" {
		return "", "", nil
	}
	joinSQL = ` LEFT JOIN actions a ON a.id = rs.action_id
	            LEFT JOIN projects p ON p.id = a.project_id`
	if tool != "" {
		whereSQL += " AND a.tool = ?"
		args = append(args, tool)
	}
	if project != "" {
		whereSQL += " AND p.root_path = ?"
		args = append(args, project)
	}
	return joinSQL, whereSQL, args
}

// compressionEventsFilter builds the optional JOIN + WHERE clauses
// for filtering compression_events by tool/project, going through
// api_turn_id → api_turns → sessions → projects.
func compressionEventsFilter(tool, project string) (joinSQL, whereSQL string, args []any) {
	if tool == "" && project == "" {
		return "", "", nil
	}
	joinSQL = ` LEFT JOIN api_turns at ON at.id = ce.api_turn_id
	            LEFT JOIN sessions s ON s.id = at.session_id
	            LEFT JOIN projects p ON p.id = s.project_id`
	if tool != "" {
		whereSQL += " AND s.tool = ?"
		args = append(args, tool)
	}
	if project != "" {
		whereSQL += " AND p.root_path = ?"
		args = append(args, project)
	}
	return joinSQL, whereSQL, args
}

// deriveThresholdHints produces tuning recommendations from the
// aggregate. Today only one hint family fires — "stash-retrieve-rate"
// — banded into three messages so the user gets something actionable
// without us pretending to know more than the data supports. Future
// hint families (size-distribution, shape-bucket) plug in here as the
// dogfood corpus grows.
//
// Bands (informed by the v1.4.43+ dogfood reframe in CCR-related
// help.js entries: CCR is "reversibility insurance, not a frequent-
// retrieve workhorse"):
//
//   - 0 retrieves on ≥10 stashes  → info: current threshold catches
//     bodies the model never returns to; raising it would shrink
//     stash dir without hurting safety. Below 10 the sample is too
//     small to recommend a change.
//   - retrieve_rate < 1%          → info: retrieves are rare; CCR
//     is doing its safety-net job. No change recommended.
//   - retrieve_rate in [1%, 5%]   → info: retrieves are happening at
//     the expected baseline. No change recommended.
//   - retrieve_rate > 5%          → consider: model returns to
//     stashed bodies often; lowering stash threshold catches
//     smaller bodies and reduces re-tool calls.
//
// Empty TotalStashes returns nil — nothing to recommend on a fresh
// install.
func deriveThresholdHints(r RetrievalRateReport) []ThresholdHint {
	if r.TotalStashes == 0 {
		return nil
	}
	rate := r.RetrieveRate
	switch {
	case r.StashRetrievals == 0 && r.TotalStashes >= 10:
		return []ThresholdHint{{
			Kind:     "stash-retrieve-rate",
			Severity: "info",
			Message: fmt.Sprintf(
				"%d stashes / 0 retrieves — current threshold catches bodies the model never returns to. Raising compression.conversation.stash.threshold_bytes (default 8192) would shrink the stash dir without hurting safety.",
				r.TotalStashes,
			),
			Metric: rate,
		}}
	case rate >= 0.05:
		return []ThresholdHint{{
			Kind:     "stash-retrieve-rate",
			Severity: "consider",
			Message: fmt.Sprintf(
				"retrieve rate %.1f%% on %d stashes — model returns to stashed content often. Lowering compression.conversation.stash.threshold_bytes captures smaller bodies the model is asking about.",
				rate*100, r.TotalStashes,
			),
			Metric: rate,
		}}
	case rate > 0:
		return []ThresholdHint{{
			Kind:     "stash-retrieve-rate",
			Severity: "info",
			Message: fmt.Sprintf(
				"retrieve rate %.2f%% on %d stashes — within expected baseline. CCR is doing its safety-net job; no threshold change recommended.",
				rate*100, r.TotalStashes,
			),
			Metric: rate,
		}}
	default:
		return nil
	}
}
