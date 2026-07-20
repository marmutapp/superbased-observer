package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// captureLimitSnapshot parses rate-limit headers off a live upstream
// response and, when at least one limit signal is present, persists a
// models.LimitSnapshot through the LimitSink (predictor limit half).
//
// No-op when no LimitSink is wired. The header read is synchronous (the
// response body has not been written yet, so resp.Header is live); the
// INSERT runs on a detached context in a goroutine so it neither blocks
// the client response nor dies when the request context cancels (same
// race insertTurnDetached closes for api_turns).
func (p *Proxy) captureLimitSnapshot(h http.Header, provider, sessionID string) {
	if p.limitSink == nil {
		return
	}
	snap, ok := parseLimitHeaders(h, provider, p.now())
	if !ok {
		return
	}
	// R4: scope keyed to the same "default" string the cache engine uses
	// proxy-side today; the real auth-identity derivation is a shared
	// follow-up (same TODO as cachetrack scope).
	snap.ScopeHash = "default"
	snap.SessionID = sessionID

	go func(s models.LimitSnapshot) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.limitSink.InsertLimitSnapshot(ctx, s); err != nil {
			p.logger.Warn("proxy: insert limit snapshot", "err", err)
		}
	}(snap)
}

// parseLimitHeaders extracts the rate-limit / subscription-window signals
// from an upstream response. Returns ok=false when none are present.
//
// ANTHROPIC HEADER SPELLINGS VERIFIED 2026-06-20 against a live
// subscription (Max/Pro) Claude Code request routed through the proxy.
// Confirmed present and correctly parsed: anthropic-ratelimit-unified-
// {5h,7d}-{utilization,reset} and anthropic-ratelimit-unified-status.
// Utilization arrives as a 0..1 fraction (e.g. 0.03); reset as absolute
// unix seconds (e.g. 1781916000). The parser stays defensive on both
// (util normalized whether 42 or 0.42; reset parsed as unix / RFC3339 /
// duration) so an upstream format change degrades gracefully.
//
// Additional unified-* headers the upstream also sends that v1 does NOT
// persist (candidate follow-ups): -representative-claim (which window is
// binding, e.g. "five_hour"), -reset (the representative reset),
// -overage-status / -overage-disabled-reason, -fallback-percentage.
//
// The Codex/OpenAI classic x-ratelimit-* spellings below remain plan-
// sourced — no live Codex request has been captured yet. The cost half
// of the predictor does not depend on any of this.
//
// http.Header.Get canonicalizes its argument, so lowercase-hyphen lookups
// match the canonical stored keys.
func parseLimitHeaders(h http.Header, provider string, now time.Time) (models.LimitSnapshot, bool) {
	snap := models.LimitSnapshot{Provider: provider, ObservedAt: now}
	var kept []string

	keep := func(key string) string {
		v := h.Get(key)
		if v != "" {
			kept = append(kept, key+"="+v)
		}
		return v
	}

	switch provider {
	case "anthropic":
		// Unified subscription windows (Max/Pro). 7d == the weekly cap.
		snap.Window5hUtil = parseUtil(keep("anthropic-ratelimit-unified-5h-utilization"))
		snap.Window5hReset = parseResetUnix(keep("anthropic-ratelimit-unified-5h-reset"), now)
		snap.Window7dUtil = parseUtil(keep("anthropic-ratelimit-unified-7d-utilization"))
		snap.Window7dReset = parseResetUnix(keep("anthropic-ratelimit-unified-7d-reset"), now)
		snap.Status = keep("anthropic-ratelimit-unified-status")
		// Classic per-minute request/token windows (also present on
		// API-key traffic, which carries no unified-* headers).
		snap.ReqLimit = parseI(keep("anthropic-ratelimit-requests-limit"))
		snap.ReqRemaining = parseI(keep("anthropic-ratelimit-requests-remaining"))
		snap.ReqReset = parseResetUnix(keep("anthropic-ratelimit-requests-reset"), now)
		snap.TokLimit = parseI(keep("anthropic-ratelimit-tokens-limit"))
		snap.TokRemaining = parseI(keep("anthropic-ratelimit-tokens-remaining"))
		snap.TokReset = parseResetUnix(keep("anthropic-ratelimit-tokens-reset"), now)
	default:
		// OpenAI / codex: classic x-ratelimit-* only. The 5h/weekly
		// ChatGPT-plan window is NOT exposed as headers (R3) — deferred
		// to a /status reader; v1 captures the per-minute windows.
		snap.ReqLimit = parseI(keep("x-ratelimit-limit-requests"))
		snap.ReqRemaining = parseI(keep("x-ratelimit-remaining-requests"))
		snap.ReqReset = parseResetUnix(keep("x-ratelimit-reset-requests"), now)
		snap.TokLimit = parseI(keep("x-ratelimit-limit-tokens"))
		snap.TokRemaining = parseI(keep("x-ratelimit-remaining-tokens"))
		snap.TokReset = parseResetUnix(keep("x-ratelimit-reset-tokens"), now)
	}

	if !snap.HasAnyWindow() {
		return snap, false
	}
	snap.Raw = strings.Join(kept, "; ")
	return snap, true
}

// parseUtil normalizes a utilization header to a 0..1 fraction. Returns
// nil when the value is empty/unparseable. Values > 1 are treated as a
// 0-100 percentage and divided by 100 (the 0-1-vs-0-100 ambiguity is the
// R1 open question; this heuristic is correct either way for in-range
// inputs).
func parseUtil(v string) *float64 {
	v = strings.TrimSpace(strings.TrimSuffix(v, "%"))
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	if f > 1 {
		f /= 100
	}
	if f < 0 {
		f = 0
	}
	return &f
}

// parseI parses an integer header into a *int64; nil on empty/unparseable.
func parseI(v string) *int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// parseResetUnix coerces a reset header to absolute unix seconds. Accepts
// a unix timestamp, an RFC3339(Nano) timestamp (Anthropic classic), or a
// Go-style duration ("6m0s", "1s" — OpenAI). nil on empty/unparseable.
func parseResetUnix(v string, now time.Time) *int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	// Bare integer: could be unix seconds or seconds-from-now. A value
	// below ~10 years of seconds is implausible as a unix timestamp, so
	// treat small integers as seconds-from-now.
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 315_360_000 { // < 10y in seconds → relative
			u := now.Add(time.Duration(n) * time.Second).Unix()
			return &u
		}
		return &n
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		u := t.Unix()
		return &u
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		u := t.Unix()
		return &u
	}
	if d, err := time.ParseDuration(v); err == nil {
		u := now.Add(d).Unix()
		return &u
	}
	return nil
}
