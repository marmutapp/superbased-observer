// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package obsalert

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// AlertRule is one alert rule + its current status (for the admin API).
type AlertRule struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Metric          string  `json:"metric"`
	Comparator      string  `json:"comparator"`
	Threshold       float64 `json:"threshold"`
	WindowDays      int     `json:"window_days"`
	WebhookURL      string  `json:"webhook_url"`
	CooldownMinutes int     `json:"cooldown_minutes"`
	Enabled         bool    `json:"enabled"`
	LastFiredAt     string  `json:"last_fired_at"`
	LastValue       float64 `json:"last_value"`
	// Breaching reports whether the last observed value currently crosses the
	// threshold (the live status, independent of cooldown).
	Breaching bool `json:"breaching"`
}

// AlertRulesResult is the GET /api/org/obs/alerts body.
type AlertRulesResult struct {
	Rules  []AlertRule     `json:"rules"`
	Events []AlertEventRow `json:"events"`
}

// AlertEventRow is one recent fire (for the admin status panel).
type AlertEventRow struct {
	RuleID    string  `json:"rule_id"`
	Metric    string  `json:"metric"`
	Threshold float64 `json:"threshold"`
	Value     float64 `json:"value"`
	Delivered bool    `json:"delivered"`
	FiredAt   string  `json:"fired_at"`
}

// NewRuleInput is the validated create payload.
type NewRuleInput struct {
	Name            string
	Metric          string
	Comparator      string
	Threshold       float64
	WindowDays      int
	WebhookURL      string
	CooldownMinutes int
}

// ValidMetric reports whether m is a known metric.
func ValidMetric(m string) bool {
	return m == MetricErrorRate || m == MetricCostUSD || m == MetricLatencyP95Ms
}

// LoadAlertRules returns all rules for the org + the most recent fire events.
func LoadAlertRules(ctx context.Context, db *sql.DB, orgID string) (AlertRulesResult, error) {
	res := AlertRulesResult{Rules: []AlertRule{}, Events: []AlertEventRow{}}
	rows, err := db.QueryContext(ctx, `
SELECT id, name, metric, comparator, threshold, window_days, COALESCE(webhook_url,''),
       cooldown_minutes, enabled, COALESCE(last_fired_at,''), last_value
  FROM obs_alert_rules WHERE org_id = ? ORDER BY created_at DESC`, orgID)
	if err != nil {
		return AlertRulesResult{}, fmt.Errorf("obsalert.LoadAlertRules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r AlertRule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.Metric, &r.Comparator, &r.Threshold, &r.WindowDays,
			&r.WebhookURL, &r.CooldownMinutes, &enabled, &r.LastFiredAt, &r.LastValue); err != nil {
			return AlertRulesResult{}, fmt.Errorf("obsalert.LoadAlertRules: scan: %w", err)
		}
		r.Enabled = enabled != 0
		r.Breaching = crossed(r.Comparator, r.LastValue, r.Threshold)
		res.Rules = append(res.Rules, r)
	}
	if err := rows.Err(); err != nil {
		return AlertRulesResult{}, err
	}

	erows, err := db.QueryContext(ctx, `
SELECT rule_id, metric, threshold, value, delivered, fired_at
  FROM obs_alert_events WHERE org_id = ? ORDER BY fired_at DESC LIMIT 20`, orgID)
	if err != nil {
		return AlertRulesResult{}, fmt.Errorf("obsalert.LoadAlertRules: events: %w", err)
	}
	defer func() { _ = erows.Close() }()
	for erows.Next() {
		var ev AlertEventRow
		var delivered int
		if err := erows.Scan(&ev.RuleID, &ev.Metric, &ev.Threshold, &ev.Value, &delivered, &ev.FiredAt); err != nil {
			return AlertRulesResult{}, fmt.Errorf("obsalert.LoadAlertRules: event scan: %w", err)
		}
		ev.Delivered = delivered != 0
		res.Events = append(res.Events, ev)
	}
	return res, erows.Err()
}

// CreateAlertRule inserts a new rule and returns its id.
func CreateAlertRule(ctx context.Context, db *sql.DB, orgID, createdBy string, in NewRuleInput, now time.Time) (string, error) {
	if !ValidMetric(in.Metric) {
		return "", fmt.Errorf("obsalert.CreateAlertRule: invalid metric %q", in.Metric)
	}
	if in.Comparator != "gt" && in.Comparator != "gte" {
		in.Comparator = "gt"
	}
	if in.WindowDays <= 0 {
		in.WindowDays = 7
	}
	if in.CooldownMinutes <= 0 {
		in.CooldownMinutes = 360
	}
	id := randID()
	if _, err := db.ExecContext(ctx, `
INSERT INTO obs_alert_rules
  (id, org_id, name, metric, comparator, threshold, window_days, webhook_url, cooldown_minutes, enabled, created_by, created_at)
 VALUES (?,?,?,?,?,?,?,?,?,1,?,?)`,
		id, orgID, in.Name, in.Metric, in.Comparator, in.Threshold, in.WindowDays, in.WebhookURL, in.CooldownMinutes,
		createdBy, now.Format(time.RFC3339)); err != nil {
		return "", fmt.Errorf("obsalert.CreateAlertRule: %w", err)
	}
	return id, nil
}

// DeleteAlertRule removes a rule (scoped to the org). Returns false when no row
// matched (so the handler can 404).
func DeleteAlertRule(ctx context.Context, db *sql.DB, orgID, id string) (bool, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM obs_alert_rules WHERE org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return false, fmt.Errorf("obsalert.DeleteAlertRule: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func randID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
