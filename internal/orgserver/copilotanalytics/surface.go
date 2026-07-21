package copilotanalytics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Surface selects which GitHub Copilot analytics API the poller targets. A tenant
// typically runs one poller per surface; copilot_analytics_daily.surface keeps
// them distinct.
type Surface string

const (
	// SurfaceEngagement is the usage-metrics report API (engagement counts; the
	// two-step download-link → NDJSON fetch). No tokens, no cost.
	SurfaceEngagement Surface = "engagement"
	// SurfaceSeats is the Copilot billing/seats API (seat COUNTS, not dollars).
	SurfaceSeats Surface = "seats"
	// SurfaceBilling is the enhanced-billing premium-request usage API ($ line
	// items; netAmount USD).
	SurfaceBilling Surface = "billing"
)

// Unit is the measurement unit of a metric's value. THE cross-vendor unit trap:
// cost is reported in different units across vendors (CC cents, Codex credits,
// OpenAI dollars, Copilot seats/usd), so it is recorded per row and normalized
// only at the read seam — and Copilot's seats/usd are NEVER summed with another
// vendor's units.
type Unit string

const (
	UnitCount  Unit = "count"  // engagement counts (suggestions, chats, users)
	UnitSeats  Unit = "seats"  // seat counts (× per-seat price at the cost read)
	UnitUSD    Unit = "usd"    // enhanced-billing netAmount (already USD)
	UnitTokens Unit = "tokens" // reserved for Rail A OTel (gated; not used here)
)

// Actor types stored in actor_type.
const (
	ActorUser       = "user"       // a GitHub login (a developer)
	ActorAutomation = "automation" // a bot/CI seat (login type=Bot or *-ci heuristic)
	ActorOrg        = "org"        // org/enterprise-aggregate row (no per-login attribution)
)

// orgAggregateKey is the user_key for rows with no per-login attribution
// (seat-breakdown totals, enhanced-billing line items).
const orgAggregateKey = "__org__"

// Metric names stored in copilot_analytics_daily. Engagement metrics are
// Copilot-exclusive (no dedup risk, never summed into a cost figure); the cost
// metrics feed the SIBLING CostSummary, never spendCTE.
const (
	// Engagement (unit: count) — Copilot-exclusive, surfaced as metrics only.
	MetricActiveUsers     = "active_users"
	MetricEngagedUsers    = "engaged_users"
	MetricCodeSuggestions = "code_suggestions"
	MetricCodeAcceptances = "code_acceptances"
	MetricLinesSuggested  = "lines_suggested"
	MetricLinesAccepted   = "lines_accepted"
	MetricChats           = "chats"

	// Seats (unit: seats / count) — the subscription baseline.
	MetricSeatsTotal    = "seats_total"    // org-aggregate seat_breakdown.total (unit: seats)
	MetricSeatsActive   = "seats_active"   // org-aggregate active_this_cycle (unit: seats)
	MetricSeatsInactive = "seats_inactive" // org-aggregate inactive_this_cycle (unit: seats)
	MetricActiveSeat    = "active_seat"    // per-login: 1 if last_activity on the day (unit: count)

	// Cost (unit: usd) — enhanced-billing metered spend (AI Credits / premium).
	MetricCost = "cost"
)

// window is a closed-open day range [Start, End) each surface formats per its own
// convention.
type window struct {
	Start time.Time
	End   time.Time
}

// DailyMetric is one normalized (day, user, surface, metric) value bound for
// copilot_analytics_daily. Surface + Unit are stamped by the surface parser.
type DailyMetric struct {
	Day       string  // YYYY-MM-DD (UTC)
	UserKey   string  // GitHub login | orgAggregateKey
	ActorType string  // ActorUser | ActorAutomation | ActorOrg
	Surface   Surface // which API produced this row
	Unit      Unit    // unit of Value
	Metric    string
	Value     float64
}

// surfaceSpec is the resolved-once strategy for one surface. The poller is
// surface-blind: it only calls poll, which owns that surface's endpoint topology
// (engagement = report-envelope → NDJSON; seats = billing+seats; billing =
// premium-request usage). Selecting the spec at construction keeps the hot path
// free of surface conditionals (rule #3).
type surfaceSpec struct {
	surface Surface
	poll    func(ctx context.Context, p *Poller, win window) ([]DailyMetric, error)
}

// surfaceRegistry is the table-driven set of supported surfaces (rule #5: a data
// table, not a conditional ladder). Adding a surface is one entry + its file.
var surfaceRegistry = map[Surface]surfaceSpec{
	SurfaceEngagement: {surface: SurfaceEngagement, poll: pollEngagement},
	SurfaceSeats:      {surface: SurfaceSeats, poll: pollSeats},
	SurfaceBilling:    {surface: SurfaceBilling, poll: pollBilling},
}

// resolveSurface returns the spec for a surface name, or an error for an unknown
// surface.
func resolveSurface(name string) (surfaceSpec, error) {
	spec, ok := surfaceRegistry[Surface(strings.TrimSpace(name))]
	if !ok {
		return surfaceSpec{}, fmt.Errorf("copilotanalytics: unknown surface %q (want %s|%s|%s)",
			name, SurfaceEngagement, SurfaceSeats, SurfaceBilling)
	}
	return spec, nil
}

// dayOf returns the YYYY-MM-DD UTC bucket for a time.
func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// utcDayFromTimestamp extracts YYYY-MM-DD (UTC) from an RFC3339 timestamp,
// falling back to the leading 10 chars if it is already date-only.
func utcDayFromTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return dayOf(t)
	}
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ""
}

// emitMetric builds a DailyMetric.
func emitMetric(day, userKey, actorType string, surface Surface, unit Unit, metric string, v float64) DailyMetric {
	return DailyMetric{
		Day: day, UserKey: userKey, ActorType: actorType,
		Surface: surface, Unit: unit, Metric: metric, Value: v,
	}
}

// actorForLogin buckets a GitHub login by a cheap automation heuristic: an
// explicit Bot type, or a "-ci"/"[bot]" suffix. A real deployment overrides this
// with the admin-supplied login map, but the heuristic keeps bot seats out of
// per-developer rollups by default.
func actorForLogin(login, ghType string) string {
	if strings.EqualFold(ghType, "Bot") {
		return ActorAutomation
	}
	l := strings.ToLower(login)
	if strings.HasSuffix(l, "-ci") || strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") {
		return ActorAutomation
	}
	return ActorUser
}

// ghHeaders sets the GitHub REST headers every authenticated request needs.
func ghHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", userAgent)
}
