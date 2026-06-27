package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Telemetry aggregates the native-console vendor-analytics tables (Claude Code
// cc_analytics_daily / Codex codex_analytics_daily / Copilot
// copilot_analytics_daily — server migrations 008/009/010) into an
// org-aggregate, content-free per-vendor summary for the window.
//
// These tables are admin-keyed and never enter the agent push wire (they are
// absent from internal/store/orgpush.go by construction), so this rollup is
// admin-only (the handler gates with requireAdmin) and takes no Scope: there is
// no per-developer attribution to scope — only per-vendor org aggregates. The
// queries select only metric/value/day/surface/unit/pulled_at — never user_key
// (the email/login actor identity), so no identity is disclosed and no sentinel
// content column is read.
//
// The cross-vendor UNIT TRAP is honored: cost is summed only within a single
// unit (USD); Codex ChatGPT-Enterprise credits are kept in CreditsCost, never
// added to dollars. Copilot seat counts are read as the latest point-in-time
// snapshot (NOT summed across days, which would double-count a subscription).
//
// Configured is false when no poller has populated any table in the window —
// the honest "native telemetry not configured" empty state for the common case.
func Telemetry(ctx context.Context, db *sql.DB, w Window, now time.Time) (TelemetryResult, error) {
	sinceDay := now.UTC().AddDate(0, 0, -w.days()).Format("2006-01-02")
	res := TelemetryResult{WindowDays: w.days(), Vendors: []VendorTelemetry{}}

	cc, err := telemetryCC(ctx, db, sinceDay)
	if err != nil {
		return TelemetryResult{}, fmt.Errorf("rollup.Telemetry: cc: %w", err)
	}
	codex, err := telemetryCodex(ctx, db, sinceDay)
	if err != nil {
		return TelemetryResult{}, fmt.Errorf("rollup.Telemetry: codex: %w", err)
	}
	copilot, err := telemetryCopilot(ctx, db, sinceDay)
	if err != nil {
		return TelemetryResult{}, fmt.Errorf("rollup.Telemetry: copilot: %w", err)
	}
	for _, v := range []*VendorTelemetry{cc, codex, copilot} {
		if v != nil {
			res.Vendors = append(res.Vendors, *v)
		}
	}
	res.Configured = len(res.Vendors) > 0
	return res, nil
}

// telemetryCC summarizes cc_analytics_daily. Metric vocabulary (the
// ccanalytics const block): cost_usd (dollars), tokens_input/output/
// cache_read/cache_creation, sessions/lines_*/commits/pull_requests, and the
// per-tool tool_<tool>_accepted/_rejected pairs that derive the accept rate.
// Returns nil when the table has no rows in the window.
func telemetryCC(ctx context.Context, db *sql.DB, sinceDay string) (*VendorTelemetry, error) {
	days, last, err := telemetryMeta(ctx, db, "cc_analytics_daily", sinceDay)
	if err != nil || days == 0 {
		return nil, err
	}
	v := &VendorTelemetry{Vendor: "claude_code", DisplayName: "Claude Code", Days: days, LastPulledAt: last}
	var buckets TokenBuckets
	var acc AcceptanceStats
	eng := map[string]int64{}
	q := `SELECT metric, COALESCE(SUM(value),0) FROM cc_analytics_daily WHERE day >= ? GROUP BY metric`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(r *sql.Rows) error {
		var metric string
		var val float64
		if err := r.Scan(&metric, &val); err != nil {
			return err
		}
		switch {
		case metric == "cost_usd":
			v.CostUSD = val
		case metric == "tokens_input":
			buckets.NetInput = int64(val)
		case metric == "tokens_output":
			buckets.Output = int64(val)
		case metric == "tokens_cache_read":
			buckets.CacheRead = int64(val)
		case metric == "tokens_cache_creation":
			buckets.CacheWrite = int64(val)
		case strings.HasSuffix(metric, "_accepted"):
			acc.Accepted += int64(val)
		case strings.HasSuffix(metric, "_rejected"):
			acc.Rejected += int64(val)
		default:
			eng[metric] += int64(val)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	v.CostUnit = costUnitLabel(v.CostUSD, 0)
	v.Tokens = nonZeroBuckets(buckets)
	if acc.Accepted+acc.Rejected > 0 {
		acc.AcceptRate = fratio(float64(acc.Accepted), float64(acc.Accepted+acc.Rejected))
		v.Acceptance = &acc
	}
	v.Engagement = engagementList(eng)
	return v, nil
}

// telemetryCodex summarizes codex_analytics_daily across both surfaces
// (chatgpt_enterprise / openai_org). Cost arrives in two units — credits
// (enterprise) and usd (openai-org) — kept distinct per the unit trap.
func telemetryCodex(ctx context.Context, db *sql.DB, sinceDay string) (*VendorTelemetry, error) {
	days, last, err := telemetryMeta(ctx, db, "codex_analytics_daily", sinceDay)
	if err != nil || days == 0 {
		return nil, err
	}
	v := &VendorTelemetry{Vendor: "codex", DisplayName: "Codex", Days: days, LastPulledAt: last}
	var buckets TokenBuckets
	var usd, credits float64
	eng := map[string]int64{}
	surfaces := map[string]struct{}{}
	q := `SELECT surface, unit, metric, COALESCE(SUM(value),0) FROM codex_analytics_daily WHERE day >= ? GROUP BY surface, unit, metric`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(r *sql.Rows) error {
		var surface, unit, metric string
		var val float64
		if err := r.Scan(&surface, &unit, &metric, &val); err != nil {
			return err
		}
		surfaces[surface] = struct{}{}
		switch {
		case metric == "cost" && unit == "usd":
			usd += val
		case metric == "cost" && unit == "credits":
			credits += val
		case metric == "tokens_input":
			buckets.NetInput += int64(val)
		case metric == "tokens_output":
			buckets.Output += int64(val)
		case metric == "tokens_cached":
			buckets.CacheRead += int64(val)
		case unit == "count":
			eng[metric] += int64(val)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	v.CostUSD = usd
	v.CreditsCost = credits
	v.CostUnit = costUnitLabel(usd, credits)
	v.Tokens = nonZeroBuckets(buckets)
	v.Engagement = engagementList(eng)
	v.Surfaces = sortedKeys(surfaces)
	return v, nil
}

// telemetryCopilot summarizes copilot_analytics_daily. Cost = the metered
// enhanced-billing overage (unit usd, surface billing); the seat subscription
// is a count-only snapshot surfaced via Seats (it needs a per-seat price the
// rollup deliberately does not assume). Engagement folds the additive count
// metrics (suggestions/acceptances/lines/chats); the point-in-time user gauges
// (active_users/engaged_users) and the per-login active_seat marker are skipped
// because summing them across days is misleading.
func telemetryCopilot(ctx context.Context, db *sql.DB, sinceDay string) (*VendorTelemetry, error) {
	days, last, err := telemetryMeta(ctx, db, "copilot_analytics_daily", sinceDay)
	if err != nil || days == 0 {
		return nil, err
	}
	v := &VendorTelemetry{Vendor: "copilot", DisplayName: "GitHub Copilot", Days: days, LastPulledAt: last}
	var overage float64
	eng := map[string]int64{}
	surfaces := map[string]struct{}{}
	q := `SELECT surface, unit, metric, COALESCE(SUM(value),0) FROM copilot_analytics_daily WHERE day >= ? GROUP BY surface, unit, metric`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(r *sql.Rows) error {
		var surface, unit, metric string
		var val float64
		if err := r.Scan(&surface, &unit, &metric, &val); err != nil {
			return err
		}
		surfaces[surface] = struct{}{}
		switch {
		case surface == "billing" && metric == "cost" && unit == "usd":
			overage += val
		case surface == "seats":
			// seat counts are point-in-time; handled via telemetryCopilotSeats.
		case unit == "count" && metric != "active_users" && metric != "engaged_users" && metric != "active_seat":
			eng[metric] += int64(val)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	v.CostUSD = overage
	v.CostUnit = costUnitLabel(overage, 0)
	v.Engagement = engagementList(eng)
	v.Surfaces = sortedKeys(surfaces)
	if v.Seats, err = telemetryCopilotSeats(ctx, db, sinceDay); err != nil {
		return nil, err
	}
	return v, nil
}

// telemetryCopilotSeats reads the latest seat-breakdown snapshot in the window
// (point-in-time — a monthly subscription, NOT additive across days). Returns
// nil when no seat snapshot exists in the window.
func telemetryCopilotSeats(ctx context.Context, db *sql.DB, sinceDay string) (*SeatStats, error) {
	var maxDay string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(day),'') FROM copilot_analytics_daily WHERE surface='seats' AND unit='seats' AND day >= ?`,
		sinceDay).Scan(&maxDay)
	if err != nil {
		return nil, err
	}
	if maxDay == "" {
		return nil, nil
	}
	st := &SeatStats{}
	q := `SELECT metric, value FROM copilot_analytics_daily WHERE surface='seats' AND unit='seats' AND day = ?`
	if err := eachRow(ctx, db, q, []any{maxDay}, func(r *sql.Rows) error {
		var metric string
		var val float64
		if err := r.Scan(&metric, &val); err != nil {
			return err
		}
		switch metric {
		case "seats_total":
			st.Total = val
		case "seats_active":
			st.Active = val
		case "seats_inactive":
			st.Inactive = val
		}
		return nil
	}); err != nil {
		return nil, err
	}
	st.Utilization = fratio(st.Active, st.Total)
	return st, nil
}

// telemetryMeta returns the distinct-day count + the most recent pulled_at for
// a vendor table in the window. days==0 means the poller has not populated it.
func telemetryMeta(ctx context.Context, db *sql.DB, table, sinceDay string) (int64, string, error) {
	//nolint:gosec // G201: table is one of three code-constant identifiers; the window binds via ?.
	q := fmt.Sprintf(`SELECT COUNT(DISTINCT day), COALESCE(MAX(pulled_at),'') FROM %s WHERE day >= ?`, table)
	var days int64
	var last string
	err := db.QueryRowContext(ctx, q, sinceDay).Scan(&days, &last)
	return days, last, err
}

// nonZeroBuckets returns a pointer to b, or nil when every bucket is zero (so
// the JSON omits an all-zero token block — an honest "no token data" empty).
func nonZeroBuckets(b TokenBuckets) *TokenBuckets {
	if b == (TokenBuckets{}) {
		return nil
	}
	return &b
}

// engagementList turns a metric→count map into a count-descending (then
// key-ascending) KeyCount slice, or nil when empty.
func engagementList(m map[string]int64) []KeyCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]KeyCount, 0, len(m))
	for k, v := range m {
		out = append(out, KeyCount{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// costUnitLabel names which cost units were present so the dashboard can be
// honest about the figure: dollars, untranslated credits, both, or none.
func costUnitLabel(usd, credits float64) string {
	switch {
	case usd > 0 && credits > 0:
		return "mixed"
	case usd > 0:
		return "usd"
	case credits > 0:
		return "credits"
	default:
		return ""
	}
}
