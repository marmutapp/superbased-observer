package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Suggestion is one org-wide, content-free advisory derived from the enriched
// Overview signals. Severity is "info" | "warn". Metric is a short human string
// (e.g. "72% on claude-opus-4-8"); there is no per-developer attribution.
type Suggestion struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Metric   string `json:"metric,omitempty"`
}

// SuggestionsResult powers GET /api/org/suggestions — the org advisor.
type SuggestionsResult struct {
	WindowDays  int          `json:"window_days"`
	Suggestions []Suggestion `json:"suggestions"`
}

// Suggestions derives org-wide hygiene/cost advisories from the enriched
// Overview (model concentration, proxy-capture share, cache reuse). It is a
// thin read-side layer over Overview — no new substrate, all content-free.
// Scoped (admin/lead) via the Overview it wraps.
func Suggestions(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (SuggestionsResult, error) {
	res := SuggestionsResult{WindowDays: w.days(), Suggestions: []Suggestion{}}
	ov, err := Overview(ctx, db, w, scope, now)
	if err != nil {
		return SuggestionsResult{}, fmt.Errorf("rollup.Suggestions: overview: %w", err)
	}

	// 1) Model concentration — one model dominating spend.
	if ov.TotalCostUSD > 0 && len(ov.ModelMix) > 0 {
		top := ov.ModelMix[0]
		for _, m := range ov.ModelMix {
			if m.CostUSD > top.CostUSD {
				top = m
			}
		}
		share := top.CostUSD / ov.TotalCostUSD
		if share >= 0.6 && top.Model != "" && top.Model != "other" {
			res.Suggestions = append(res.Suggestions, Suggestion{
				ID:       "model_concentration",
				Severity: "info",
				Title:    "Spend is concentrated on one model",
				Detail:   "A single model drives most of the org's spend. Routing trivial / read-heavy turns to a cheaper sibling (see the Routing page) can cut cost with little quality loss.",
				Metric:   fmt.Sprintf("%.0f%% on %s", share*100, top.Model),
			})
		}
	}

	// 2) Proxy capture — a large estimated-cost share means most traffic
	// bypassed the local proxy, so latency/cache/exact-cost are unavailable.
	if ov.TotalCostUSD > 0 {
		if ov.TotalAPITurns == 0 {
			res.Suggestions = append(res.Suggestions, Suggestion{
				ID:       "not_proxied",
				Severity: "warn",
				Title:    "No proxy capture in this window",
				Detail:   "All cost is estimated from session logs. Routing nodes through the local proxy (ANTHROPIC_BASE_URL / OPENAI_BASE_URL) yields exact per-turn cost, latency, and cache visibility.",
				Metric:   "0 proxy turns",
			})
		} else if ov.Reliability != nil && ov.Reliability.ProxyShare < 0.6 {
			res.Suggestions = append(res.Suggestions, Suggestion{
				ID:       "low_proxy_share",
				Severity: "info",
				Title:    "Much of the cost is estimated, not measured",
				Detail:   "A minority of cost came through the proxy. The more nodes route through it, the more of the dashboard's dollars are measured rather than inferred.",
				Metric:   fmt.Sprintf("%.0f%% proxy-measured", ov.Reliability.ProxyShare*100),
			})
		}
	}

	// 3) Cache reuse — significant cache writes with a low read/write ratio
	// means the org is paying to cache without recouping it across reads.
	if ov.Cache != nil && ov.Cache.WriteTokens > 0 && ov.Cache.ReadWriteRatio < 1.0 {
		res.Suggestions = append(res.Suggestions, Suggestion{
			ID:       "low_cache_reuse",
			Severity: "info",
			Title:    "Low prompt-cache reuse",
			Detail:   "Cache writes are not being recouped across enough reads. Stable, long-lived system prompts and fewer tool/MCP churn events improve reuse (see the per-session Cache signals).",
			Metric:   fmt.Sprintf("%.2f× read/write", ov.Cache.ReadWriteRatio),
		})
	}

	return res, nil
}
