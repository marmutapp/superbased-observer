package advisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// State statuses.
const (
	StatusDismissed = "dismissed"
	StatusSnoozed   = "snoozed"
	StatusActed     = "acted"
)

// SetState records a user decision on a suggestion's dedup_key.
// snoozedUntil applies only to StatusSnoozed (zero time otherwise).
func SetState(ctx context.Context, db *sql.DB, dedupKey, status string, snoozedUntil time.Time, now time.Time) error {
	switch status {
	case StatusDismissed, StatusSnoozed, StatusActed:
	default:
		return fmt.Errorf("advisor.SetState: unknown status %q", status)
	}
	var su any
	if status == StatusSnoozed && !snoozedUntil.IsZero() {
		su = snoozedUntil.UTC().Format(time.RFC3339)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO advisor_state (dedup_key, status, snoozed_until, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(dedup_key) DO UPDATE SET
			status = excluded.status,
			snoozed_until = excluded.snoozed_until,
			updated_at = excluded.updated_at`,
		dedupKey, status, su, now.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("advisor.SetState: %w", err)
	}
	return nil
}

// loadMuted returns the dedup_keys the engine must hide right now:
// dismissed/acted keys inside the cooldown window, and snoozed keys whose
// snooze hasn't elapsed.
func loadMuted(ctx context.Context, db *sql.DB, now time.Time, cooldownDays int) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT dedup_key, status, COALESCE(snoozed_until,''), updated_at FROM advisor_state`)
	if err != nil {
		return nil, fmt.Errorf("advisor.loadMuted: %w", err)
	}
	defer rows.Close()
	muted := map[string]bool{}
	for rows.Next() {
		var key, status, su, updated string
		if err := rows.Scan(&key, &status, &su, &updated); err != nil {
			return nil, fmt.Errorf("advisor.loadMuted: scan: %w", err)
		}
		switch status {
		case StatusSnoozed:
			if t, ok := parseTS(su); ok && now.Before(t) {
				muted[key] = true
			}
		case StatusDismissed, StatusActed:
			if t, ok := parseTS(updated); ok && now.Before(t.AddDate(0, 0, cooldownDays)) {
				muted[key] = true
			}
		}
	}
	return muted, rows.Err()
}

// SaveDigest snapshots the report's top suggestions into the one-row
// advisor_digest table for latency-bound consumers (session-start hook,
// MCP tool) to point-read. topK bounds payload size.
func SaveDigest(ctx context.Context, db *sql.DB, rep Report, topK int) error {
	if topK <= 0 {
		topK = 5
	}
	if len(rep.Suggestions) > topK {
		rep.Suggestions = rep.Suggestions[:topK]
	}
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("advisor.SaveDigest: marshal: %w", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO advisor_digest (id, generated_at, payload)
		VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET generated_at = excluded.generated_at, payload = excluded.payload`,
		rep.GeneratedAt, string(body))
	if err != nil {
		return fmt.Errorf("advisor.SaveDigest: %w", err)
	}
	return nil
}

// LoadDigest point-reads the digest snapshot. Returns (zero, false, nil)
// when no digest has been written yet.
func LoadDigest(ctx context.Context, db *sql.DB) (Report, bool, error) {
	var payload string
	err := db.QueryRowContext(ctx, `SELECT payload FROM advisor_digest WHERE id = 1`).Scan(&payload)
	if err == sql.ErrNoRows {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, fmt.Errorf("advisor.LoadDigest: %w", err)
	}
	var rep Report
	if err := json.Unmarshal([]byte(payload), &rep); err != nil {
		return Report{}, false, fmt.Errorf("advisor.LoadDigest: unmarshal: %w", err)
	}
	return rep, true, nil
}
