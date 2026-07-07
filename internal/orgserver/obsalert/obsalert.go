// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

// Package obsalert evaluates admin-authored observability alert rules
// (obs-org-tier plan §6 / OP6b) over the content-free obs_summaries aggregate
// and fires a webhook when a metric crosses its threshold. It mirrors the
// budget Evaluator's loop + delivery shape but alerts on custom-app / agent
// TRAJECTORY health (error rate / cost / p95 latency), DISTINCT from the
// api_turns-based budget caps. Read-only over obs data; the only writes are the
// rule's last_fired_at/last_value high-water mark and the obs_alert_events log.
package obsalert

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// Metric names (the closed vocabulary an alert rule evaluates).
const (
	MetricErrorRate    = "error_rate"     // sum(error_traces)/sum(traces), 0..1
	MetricCostUSD      = "cost_usd"       // sum(cost_usd) over the window
	MetricLatencyP95Ms = "latency_p95_ms" // p95 span latency (needs shared T2 spans)
)

// Alert is the webhook payload delivered on a threshold crossing.
type Alert struct {
	RuleID     string    `json:"rule_id"`
	RuleName   string    `json:"rule_name"`
	OrgID      string    `json:"org_id"`
	OrgName    string    `json:"org_name"`
	Metric     string    `json:"metric"`
	Threshold  float64   `json:"threshold"`
	Value      float64   `json:"value"`
	WindowDays int       `json:"window_days"`
	FiredAt    time.Time `json:"fired_at"`
}

// Evaluator polls obs_alert_rules and fires webhooks on threshold crossings.
type Evaluator struct {
	db       *sql.DB
	org      orgdb.Org
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
	// Deliver posts an alert to a webhook; swappable in tests. Defaults to a
	// bounded HTTP POST.
	Deliver func(ctx context.Context, url string, a Alert) error
}

// NewEvaluator builds the obs-alert evaluator. interval ≤ 0 defaults to 5m.
func NewEvaluator(db *sql.DB, org orgdb.Org, interval time.Duration, logger *slog.Logger) *Evaluator {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	e := &Evaluator{db: db, org: org, interval: interval, logger: logger, now: func() time.Time { return time.Now().UTC() }}
	e.Deliver = e.deliver
	return e
}

// Run evaluates immediately, then on every interval tick until ctx is done.
func (e *Evaluator) Run(ctx context.Context) {
	e.logger.Info("obs-alert evaluator started", "interval", e.interval.String())
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		if err := e.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
			e.logger.Warn("obs-alert evaluate failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type rule struct {
	id, name, metric, comparator, webhook string
	threshold                             float64
	windowDays, cooldownMin               int
	lastFired                             string
}

// EvaluateOnce evaluates every enabled rule once.
func (e *Evaluator) EvaluateOnce(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx, `
SELECT id, name, metric, comparator, threshold, window_days, COALESCE(webhook_url,''),
       cooldown_minutes, COALESCE(last_fired_at,'')
  FROM obs_alert_rules WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("obsalert.EvaluateOnce: list: %w", err)
	}
	var rules []rule
	for rows.Next() {
		var r rule
		if err := rows.Scan(&r.id, &r.name, &r.metric, &r.comparator, &r.threshold,
			&r.windowDays, &r.webhook, &r.cooldownMin, &r.lastFired); err != nil {
			_ = rows.Close()
			return fmt.Errorf("obsalert.EvaluateOnce: scan: %w", err)
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	now := e.now()
	for _, r := range rules {
		val, err := e.metricValue(ctx, r)
		if err != nil {
			e.logger.Error("obs-alert metric", "rule", r.id, "err", err)
			continue
		}
		// Persist the observed value regardless of firing (for the UI status).
		_, _ = e.db.ExecContext(ctx, `UPDATE obs_alert_rules SET last_value = ? WHERE id = ?`, val, r.id)

		if !crossed(r.comparator, val, r.threshold) {
			continue
		}
		if inCooldown(r.lastFired, r.cooldownMin, now) {
			continue
		}
		delivered := true
		if r.webhook != "" {
			if derr := e.Deliver(ctx, r.webhook, e.alert(r, val, now)); derr != nil {
				delivered = false
				e.logger.Warn("obs-alert webhook failed", "rule", r.id, "err", derr)
			}
		}
		if _, err := e.db.ExecContext(ctx,
			`UPDATE obs_alert_rules SET last_fired_at = ? WHERE id = ?`, now.Format(time.RFC3339), r.id); err != nil {
			e.logger.Error("obs-alert persist last_fired", "rule", r.id, "err", err)
		}
		if _, err := e.db.ExecContext(ctx,
			`INSERT INTO obs_alert_events (rule_id, org_id, metric, threshold, value, delivered, fired_at)
			 VALUES (?,?,?,?,?,?,?)`,
			r.id, e.org.OrgID, r.metric, r.threshold, val, boolToInt(delivered), now.Format(time.RFC3339)); err != nil {
			e.logger.Error("obs-alert log event", "rule", r.id, "err", err)
		}
		e.logger.Info("obs-alert fired", "rule", r.id, "metric", r.metric, "value", val, "threshold", r.threshold, "delivered", delivered)
	}
	return nil
}

// metricValue computes the rule's metric over its window via ObsAnalytics.
func (e *Evaluator) metricValue(ctx context.Context, r rule) (float64, error) {
	win := rollup.Window{Days: r.windowDays}
	res, err := rollup.ObsAnalytics(ctx, e.db, win, e.now())
	if err != nil {
		return 0, err
	}
	switch r.metric {
	case MetricErrorRate:
		return res.ErrorRate, nil
	case MetricCostUSD:
		return res.TotalCostUSD, nil
	case MetricLatencyP95Ms:
		return float64(res.LatencyP95Ms), nil
	default:
		return 0, fmt.Errorf("unknown metric %q", r.metric)
	}
}

func (e *Evaluator) alert(r rule, val float64, now time.Time) Alert {
	return Alert{
		RuleID: r.id, RuleName: r.name, OrgID: e.org.OrgID, OrgName: e.org.OrgName,
		Metric: r.metric, Threshold: r.threshold, Value: val, WindowDays: r.windowDays, FiredAt: now,
	}
}

// crossed reports whether val crosses the threshold under the comparator.
func crossed(comparator string, val, threshold float64) bool {
	if comparator == "gte" {
		return val >= threshold
	}
	return val > threshold
}

// inCooldown reports whether the rule fired within cooldownMin of now.
func inCooldown(lastFired string, cooldownMin int, now time.Time) bool {
	if lastFired == "" || cooldownMin <= 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastFired)
	if err != nil {
		return false
	}
	return now.Sub(t) < time.Duration(cooldownMin)*time.Minute
}

// deliver posts the alert JSON to the webhook with a bounded timeout.
func (e *Evaluator) deliver(ctx context.Context, url string, a Alert) error {
	body, _ := json.Marshal(a)
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
