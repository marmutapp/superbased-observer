package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RateLimitWindows is the subscription-window utilization for a tool that
// captures its 5h / weekly rate-limit state in its own transcript rather
// than in HTTP response headers. Codex 0.130+ embeds
// `rate_limits.{primary,secondary}` in every token_count event, which the
// codex adapter persists as ActionRateLimit rows; this is the read side
// that turns those rows into the same gauge shape the proxy snapshot
// produces. Optional fields are nil when the source omitted that window.
type RateLimitWindows struct {
	Window5hUtil  *float64 // primary window, 0..1
	Window5hReset *int64   // primary resets_at, unix seconds
	Window7dUtil  *float64 // secondary window, 0..1
	Window7dReset *int64   // secondary resets_at, unix seconds
	PlanType      string   // "plus" / "pro" / "team"
	Status        string   // rate_limit_reached_type, "" when not throttled
	ObservedAt    time.Time
}

// rateLimitActionRaw mirrors the JSON the codex adapter marshals into
// actions.raw_tool_input for an ActionRateLimit row (the codexRateLimits
// envelope). Declared locally so this read seam stays decoupled from the
// adapter package — capabilities, not source identity.
type rateLimitActionRaw struct {
	LimitID              string           `json:"limit_id"`
	Primary              *rateLimitWindow `json:"primary"`
	Secondary            *rateLimitWindow `json:"secondary"`
	PlanType             string           `json:"plan_type"`
	RateLimitReachedType *string          `json:"rate_limit_reached_type"`
}

type rateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// LatestRateLimitWindows returns the most-recent subscription-window
// snapshot captured from a tool's own transcript (ActionRateLimit rows),
// session-scoped first and falling back to the latest for the tool
// account-wide (the window ticks per-account, so a brand-new session with
// no rate_limit row yet still gets a reading). ok=false when the tool
// emits no such rows (every non-codex tool today). Never errors on a
// malformed body — it just skips to the account-wide fallback / returns
// ok=false, since this only feeds an advisory gauge.
func (s *Store) LatestRateLimitWindows(ctx context.Context, tool, sessionID string) (RateLimitWindows, bool, error) {
	if sessionID != "" {
		if w, ok, err := s.latestRateLimitRow(ctx,
			`SELECT raw_tool_input, timestamp FROM actions
			  WHERE session_id = ? AND action_type = 'rate_limit'
			    AND raw_tool_input IS NOT NULL AND raw_tool_input <> ''
			  ORDER BY timestamp DESC, id DESC LIMIT 1`, sessionID); err != nil {
			return RateLimitWindows{}, false, err
		} else if ok {
			return w, true, nil
		}
	}
	if tool == "" {
		return RateLimitWindows{}, false, nil
	}
	return s.latestRateLimitRow(ctx,
		`SELECT raw_tool_input, timestamp FROM actions
		  WHERE tool = ? AND action_type = 'rate_limit'
		    AND raw_tool_input IS NOT NULL AND raw_tool_input <> ''
		  ORDER BY timestamp DESC, id DESC LIMIT 1`, tool)
}

func (s *Store) latestRateLimitRow(ctx context.Context, query string, args ...any) (RateLimitWindows, bool, error) {
	var raw, ts string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&raw, &ts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RateLimitWindows{}, false, nil
		}
		return RateLimitWindows{}, false, fmt.Errorf("store.LatestRateLimitWindows: %w", err)
	}
	w, ok := parseRateLimitWindows(raw)
	if !ok {
		// Malformed body: not an error, just no usable window.
		return RateLimitWindows{}, false, nil
	}
	if t, ok := parseDBTime(ts); ok {
		w.ObservedAt = t
	}
	return w, true, nil
}

// parseRateLimitWindows decodes the codexRateLimits envelope into the
// gauge shape. used_percent is 0..100 → util 0..1. Returns ok=false when
// neither window is present.
func parseRateLimitWindows(raw string) (RateLimitWindows, bool) {
	var rl rateLimitActionRaw
	if err := json.Unmarshal([]byte(raw), &rl); err != nil {
		return RateLimitWindows{}, false
	}
	var w RateLimitWindows
	w.PlanType = rl.PlanType
	if rl.RateLimitReachedType != nil {
		w.Status = *rl.RateLimitReachedType
	}
	present := false
	if rl.Primary != nil {
		util := rl.Primary.UsedPercent / 100.0
		w.Window5hUtil = &util
		if rl.Primary.ResetsAt > 0 {
			r := rl.Primary.ResetsAt
			w.Window5hReset = &r
		}
		present = true
	}
	if rl.Secondary != nil {
		util := rl.Secondary.UsedPercent / 100.0
		w.Window7dUtil = &util
		if rl.Secondary.ResetsAt > 0 {
			r := rl.Secondary.ResetsAt
			w.Window7dReset = &r
		}
		present = true
	}
	return w, present
}
