package dashboard

import (
	"net/http"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Budget guardrails v2 (usability arc P6.3 / review §8.2): month-to-
// date spend vs the operator's self-set budgets — global and
// per-project — with a linear month-end forecast and 80/100%
// threshold levels. ADVISORY ONLY by design (P1): this surface renders
// banners and progress bars; nothing here, or anywhere else, gates
// proxy traffic.
//
// Budgets are read FRESH from config.toml on every request (the
// loadConfigForDashboard pattern), so edits from the Cost page's
// Budget card apply on the next poll without a daemon restart.

// budgetScope is the budget state for one scope (global or a project).
type budgetScope struct {
	Root        string  `json:"root,omitempty"`
	BudgetUSD   float64 `json:"budget_usd"`
	MTDUSD      float64 `json:"mtd_usd"`
	Pct         float64 `json:"pct"`
	ForecastUSD float64 `json:"forecast_usd"`
	// Threshold is "" (under 80%), "warn80", or "over100" — the
	// banner's firing level.
	Threshold string `json:"threshold,omitempty"`
}

// handleBudget serves GET /api/budget. When no budget is configured
// anywhere it reports configured=false after one config read — zero
// DB work for installs that never set a budget (the banner polls
// this).
func (s *Server) handleBudget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	globalBudget := s.opts.MonthlyBudgetUSD
	var projectBudgets map[string]float64
	if cfg, err := loadConfigForDashboard(s.opts.ConfigPath); err == nil {
		globalBudget = cfg.Intelligence.MonthlyBudgetUSD
		projectBudgets = cfg.Intelligence.ProjectBudgetsUSD
	}
	configured := globalBudget > 0
	for _, b := range projectBudgets {
		if b > 0 {
			configured = true
		}
	}

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	daysElapsed := int(now.Sub(monthStart).Hours()/24) + 1
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()

	resp := map[string]any{
		"configured":    configured,
		"month":         now.Format("2006-01"),
		"days_elapsed":  daysElapsed,
		"days_in_month": daysInMonth,
	}
	if !configured || s.db() == nil {
		writeJSON(w, resp)
		return
	}

	globalMTD, perRoot, err := s.monthToDateCostByProject(r, monthStart)
	if err != nil {
		writeErr(w, err)
		return
	}

	project := float64(daysInMonth) / float64(daysElapsed)
	if globalBudget > 0 {
		resp["global"] = scopeFor("", globalBudget, globalMTD, project)
	}
	projects := []budgetScope{}
	for root, budget := range projectBudgets {
		if budget <= 0 {
			continue
		}
		projects = append(projects, scopeFor(root, budget, perRoot[root], project))
	}
	// Deterministic order: highest budget consumption first.
	for i := 1; i < len(projects); i++ {
		for j := i; j > 0 && projects[j].Pct > projects[j-1].Pct; j-- {
			projects[j], projects[j-1] = projects[j-1], projects[j]
		}
	}
	resp["projects"] = projects
	writeJSON(w, resp)
}

// scopeFor assembles one budget scope with pct, linear forecast, and
// threshold level.
func scopeFor(root string, budget, mtd, projection float64) budgetScope {
	sc := budgetScope{
		Root:        root,
		BudgetUSD:   budget,
		MTDUSD:      mtd,
		Pct:         mtd / budget * 100,
		ForecastUSD: mtd * projection,
	}
	switch {
	case sc.Pct >= 100:
		sc.Threshold = "over100"
	case sc.Pct >= 80:
		sc.Threshold = "warn80"
	}
	return sc
}

// monthToDateCostByProject returns the month-to-date spend, total and
// grouped by project root. Same proxy-dedup discipline as the cost
// surfaces (api_turns win; token_usage rows matching a proxy
// request_id are skipped); project attribution joins through sessions
// for both arms so the two sources bucket identically. Rows without a
// resolvable project count toward the global total only.
func (s *Server) monthToDateCostByProject(r *http.Request, monthStart time.Time) (float64, map[string]float64, error) {
	since := monthStart.Format(time.RFC3339Nano)
	rows, err := s.db().QueryContext(r.Context(),
		`WITH proxy_turn_ids AS (
			SELECT request_id FROM api_turns
			 WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ?
		),
		combined AS (
			SELECT at.session_id, at.model, at.input_tokens, at.output_tokens,
			       at.cache_read_tokens, at.cache_creation_tokens, at.cache_creation_1h_tokens,
			       0 AS reasoning_tokens, at.web_search_requests, at.cost_usd
			FROM api_turns at
			WHERE at.timestamp >= ?
			UNION ALL
			SELECT tu.session_id, tu.model, tu.input_tokens, tu.output_tokens,
			       tu.cache_read_tokens, tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
			       tu.reasoning_tokens, tu.web_search_requests, tu.estimated_cost_usd
			FROM token_usage tu
			WHERE tu.timestamp >= ?
			  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
			       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
		)
		SELECT COALESCE(p.root_path, ''),
		       COALESCE(c.model, ''),
		       COALESCE(c.input_tokens, 0), COALESCE(c.output_tokens, 0),
		       COALESCE(c.cache_read_tokens, 0), COALESCE(c.cache_creation_tokens, 0),
		       COALESCE(c.cache_creation_1h_tokens, 0), COALESCE(c.reasoning_tokens, 0),
		       COALESCE(c.web_search_requests, 0), COALESCE(c.cost_usd, 0)
		FROM combined c
		LEFT JOIN sessions s ON s.id = c.session_id
		LEFT JOIN projects p ON p.id = s.project_id`,
		since, since, since)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	var total float64
	perRoot := map[string]float64{}
	for rows.Next() {
		var (
			root   string
			model  string
			bundle cost.TokenBundle
			rec    float64
		)
		if rows.Scan(&root, &model, &bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning, &bundle.WebSearchRequests, &rec) != nil {
			continue
		}
		rowCost := rec
		if rowCost <= 0 && s.opts.CostEngine != nil {
			if p, ok := s.opts.CostEngine.Lookup(model); ok {
				rowCost = cost.Compute(p, bundle)
			}
		}
		total += rowCost
		if root != "" {
			perRoot[root] += rowCost
		}
	}
	return total, perRoot, rows.Err()
}
