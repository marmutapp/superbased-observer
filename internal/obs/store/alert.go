package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/alert"
)

// AlertSummary computes the content-free alert metrics over the window ending
// at now (windowMinutes back): error_rate (error traces / total traces),
// cost_usd (summed span cost), and latency_p95_ms (p95 of span wall-durations).
// It is the node-side analogue of the org rollup.ObsAnalytics metrics the org
// obsalert evaluator compares against, read over THIS node's local obs_* tables
// so a node with sharing off still has a metric to threshold. windowMinutes<=0
// defaults to 60.
func (s *Store) AlertSummary(ctx context.Context, windowMinutes int, now time.Time) (alert.Summary, error) {
	if windowMinutes <= 0 {
		windowMinutes = 60
	}
	since := now.UTC().Add(-time.Duration(windowMinutes) * time.Minute).Format(time.RFC3339Nano)

	var out alert.Summary

	// error_rate: traces with status='error' over all traces in the window
	// (by started_at). Mirrors the org obs_summaries error_traces/traces ratio.
	var total, errTraces int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END), 0)
  FROM obs_traces
 WHERE started_at >= ?`, since).Scan(&total, &errTraces); err != nil {
		return alert.Summary{}, fmt.Errorf("obs/store.AlertSummary: error rate: %w", err)
	}
	if total > 0 {
		out.ErrorRate = float64(errTraces) / float64(total)
	}

	// cost_usd: summed span cost over the window.
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(cost_usd), 0) FROM obs_spans WHERE started_at >= ?`, since).Scan(&out.CostUSD); err != nil {
		return alert.Summary{}, fmt.Errorf("obs/store.AlertSummary: cost: %w", err)
	}

	// latency_p95_ms: p95 of span wall-durations. The node obs_spans schema has
	// no duration column (durations are derived from timestamps, as read.go
	// does), so load the window's spans and compute the percentile in Go.
	rows, err := s.db.QueryContext(ctx, `
SELECT started_at, COALESCE(ended_at, '') FROM obs_spans
 WHERE started_at >= ? ORDER BY started_at DESC LIMIT 200000`, since)
	if err != nil {
		return alert.Summary{}, fmt.Errorf("obs/store.AlertSummary: latency: %w", err)
	}
	defer rows.Close()
	var durs []int64
	for rows.Next() {
		var start, end string
		if err := rows.Scan(&start, &end); err != nil {
			return alert.Summary{}, fmt.Errorf("obs/store.AlertSummary: latency scan: %w", err)
		}
		if d := durationMS(start, end); d > 0 {
			durs = append(durs, d)
		}
	}
	if err := rows.Err(); err != nil {
		return alert.Summary{}, fmt.Errorf("obs/store.AlertSummary: latency rows: %w", err)
	}
	out.LatencyP95Ms = percentileInt64(durs, 0.95)
	return out, nil
}

// LastAlertFired returns the most-recent fire time for the named rule (the
// cooldown/dedup anchor) and ok=false when the rule has never fired. Node rules
// live in config, not a DB row, so the last-fired needed for cooldown is
// derived from the obs_alert_events log (one owner, one table).
func (s *Store) LastAlertFired(ctx context.Context, ruleName string) (time.Time, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(fired_at), '') FROM obs_alert_events WHERE rule_name = ?`, ruleName).Scan(&raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("obs/store.LastAlertFired: %w", err)
	}
	if raw == "" {
		return time.Time{}, false, nil
	}
	t, perr := time.Parse(time.RFC3339Nano, raw)
	if perr != nil {
		return time.Time{}, false, nil // unparseable ⇒ treat as never fired
	}
	return t, true, nil
}

// InsertAlertEvent records one fired alert (the dedup/audit log + the CLI's
// "recent fired" source). delivered reflects whether the webhook POST
// succeeded (false when no webhook is configured or delivery failed).
func (s *Store) InsertAlertEvent(ctx context.Context, f alert.Fired, delivered bool) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO obs_alert_events (rule_name, metric, comparator, threshold, value, window_minutes, delivered, fired_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.RuleName, f.Metric, f.Comparator, f.Threshold, f.Value, f.WindowMinutes,
		boolToInt(delivered), f.FiredAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("obs/store.InsertAlertEvent: %w", err)
	}
	return nil
}

// AlertEventRow is one persisted fired-alert row for the CLI/status surface.
type AlertEventRow struct {
	RuleName      string
	Metric        string
	Comparator    string
	Threshold     float64
	Value         float64
	WindowMinutes int
	Delivered     bool
	FiredAt       time.Time
}

// RecentAlertEvents returns the most-recent fired alerts (newest first), up to
// limit (clamped to [1,500]).
func (s *Store) RecentAlertEvents(ctx context.Context, limit int) ([]AlertEventRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT rule_name, metric, comparator, threshold, value, window_minutes, delivered, fired_at
  FROM obs_alert_events ORDER BY fired_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("obs/store.RecentAlertEvents: %w", err)
	}
	defer rows.Close()
	var out []AlertEventRow
	for rows.Next() {
		var r AlertEventRow
		var delivered int
		var firedAt string
		if err := rows.Scan(&r.RuleName, &r.Metric, &r.Comparator, &r.Threshold,
			&r.Value, &r.WindowMinutes, &delivered, &firedAt); err != nil {
			return nil, fmt.Errorf("obs/store.RecentAlertEvents: scan: %w", err)
		}
		r.Delivered = delivered != 0
		if t, perr := time.Parse(time.RFC3339Nano, firedAt); perr == nil {
			r.FiredAt = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// percentileInt64 returns the p-quantile (0..1) of vals using the nearest-rank
// method (vals is copied, not mutated). Returns 0 for an empty slice. Mirrors
// rollup.percentile so node and org latency percentiles agree.
func percentileInt64(vals []int64, p float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]int64(nil), vals...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(p * float64(len(cp)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// boolToInt maps a bool to 0/1 for the delivered column.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
