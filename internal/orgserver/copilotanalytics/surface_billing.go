package copilotanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// pollBilling reads enhanced-billing premium-request usage ($ line items) and
// sums netAmount per day into a single cost metric (unit usd). This is the
// post-June-2026 AI-Credits / premium-request metered spend — already USD, read
// not recomputed (do NOT multiply by a hardcoded rate; findings §4.2). Line items
// are org/account-level (no per-login attribution), so they land under
// orgAggregateKey and feed the SIBLING CostSummary, never spendCTE.
//
// RESIDUAL (findings Q-C4): the exact product/sku/unitType strings are
// doc-reconstructed; lock them against a live payload. We match product=="copilot"
// case-insensitively and sum netAmount; if the live strings differ, only the
// product filter changes.
func pollBilling(ctx context.Context, p *Poller, win window) ([]DailyMetric, error) {
	perDay := map[string]float64{}
	for _, ym := range monthsInWindow(win) {
		items, err := p.fetchPremiumRequestUsage(ctx, ym.year, ym.month)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if !strings.EqualFold(strings.TrimSpace(it.Product), "copilot") {
				continue
			}
			day := utcDayFromTimestamp(it.Date)
			if day == "" {
				continue
			}
			// Keep only days inside the polled window.
			if t, err := time.Parse("2006-01-02", day); err == nil {
				if t.Before(win.Start.Truncate(24*time.Hour)) || !t.Before(win.End) {
					continue
				}
			}
			perDay[day] += it.NetAmount
		}
	}
	var out []DailyMetric
	for day, usd := range perDay {
		out = append(out, emitMetric(day, orgAggregateKey, ActorOrg, SurfaceBilling, UnitUSD, MetricCost, usd))
	}
	return out, nil
}

// billingUsageResponse is the enhanced-billing usage envelope.
type billingUsageResponse struct {
	UsageItems []billingUsageItem `json:"usageItems"`
}

// billingUsageItem is one $ line item. netAmount is USD.
type billingUsageItem struct {
	Date      string  `json:"date"`
	Product   string  `json:"product"`
	SKU       string  `json:"sku"`
	Model     string  `json:"model"`
	UnitType  string  `json:"unitType"`
	NetAmount float64 `json:"netAmount"`
}

func (p *Poller) fetchPremiumRequestUsage(ctx context.Context, year, month int) ([]billingUsageItem, error) {
	q := url.Values{}
	q.Set("year", fmt.Sprintf("%d", year))
	q.Set("month", fmt.Sprintf("%d", month))
	rawURL := p.BaseURL + p.billingPrefix() + "/settings/billing/premium_request/usage?" + q.Encode()
	body, _, err := p.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	var r billingUsageResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("copilotanalytics: parse billing usage: %w", err)
	}
	return r.UsageItems, nil
}

// yearMonth identifies a billing month.
type yearMonth struct{ year, month int }

// monthsInWindow returns the distinct (year, month) buckets the window touches,
// so the per-month billing endpoint is queried once per relevant month.
func monthsInWindow(win window) []yearMonth {
	seen := map[yearMonth]bool{}
	var out []yearMonth
	for d := win.Start.UTC().Truncate(24 * time.Hour); d.Before(win.End); d = d.AddDate(0, 0, 1) {
		ym := yearMonth{d.Year(), int(d.Month())}
		if !seen[ym] {
			seen[ym] = true
			out = append(out, ym)
		}
	}
	return out
}
