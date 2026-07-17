package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/marmutapp/superbased-observer/internal/store"
)

// verbosity_aggregate.go serves GET /api/verbosity/aggregate?by=model|project|day&since_days=N
// — the Output Composition (Verbosity) ANALYSIS page (plan §6). Cross-session
// rollup: code vs explanation BYTES per group (the exact primary unit) plus an
// est total $ per group priced per-(group×model) so mixed-model project/day
// groups stay honest. The per-bucket code/explain $ split stays on the single-
// session card (one model → one rate); aggregate groups report total $ only.

// VerbosityAggregateResponse is the page payload.
type VerbosityAggregateResponse struct {
	By        string                    `json:"by"`
	SinceDays int                       `json:"since_days"`
	Groups    []VerbosityAggregateGroup `json:"groups"`
}

// VerbosityAggregateGroup is one dimension bucket (a model / project / day).
type VerbosityAggregateGroup struct {
	Key              string      `json:"key"`
	CodeBytes        int64       `json:"code_bytes"`
	ExplainBytes     int64       `json:"explain_bytes"`
	TotalBytes       int64       `json:"total_bytes"`
	CodePct          float64     `json:"code_pct"`
	ExplainPct       float64     `json:"explain_pct"`
	CodeExplainRatio *float64    `json:"code_explain_ratio,omitempty"`
	TopLanguages     []langBytes `json:"top_languages"`
	// EstTotalUSD is the est output+reasoning spend for the group, priced
	// per-model. CostEstimated is false when no model in the group is priced
	// (the surface then shows "—" rather than $0).
	EstTotalUSD   float64 `json:"est_total_usd,omitempty"`
	CostEstimated bool    `json:"cost_estimated"`
}

const verbosityAggregateMaxLangs = 8

func (s *Server) handleVerbosityAggregate(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "model"
	}
	sinceDays := 30
	if v := r.URL.Query().Get("since_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			sinceDays = n
		}
	}
	ctx := r.Context()
	st := store.New(s.opts.DB)

	groups, err := st.LoadVerbosityAggregate(ctx, by, sinceDays)
	if err != nil {
		// Unknown dimension is the only expected error → 400, not 500.
		http.Error(w, fmt.Sprintf("verbosity aggregate: %v", err), http.StatusBadRequest)
		return
	}

	// Est $ per group: sum each (group, model) token total priced at the
	// model's output rate. Best-effort — a query/pricing miss just omits $.
	costByKey := map[string]float64{}
	pricedKey := map[string]bool{}
	if toks, terr := st.LoadVerbosityGroupTokens(ctx, by, sinceDays); terr == nil {
		for _, t := range toks {
			if rates, ok := lookupRates(s.opts.CostEngine, t.Model); ok && rates.Output > 0 {
				costByKey[t.Key] += float64(t.Output+t.Reasoning) * rates.Output
				pricedKey[t.Key] = true
			}
		}
	}

	resp := VerbosityAggregateResponse{By: by, SinceDays: sinceDays, Groups: make([]VerbosityAggregateGroup, 0, len(groups))}
	for _, g := range groups {
		b := g.Breakdown
		code, explain := b.CodeBytes(), b.ExplainBytes()
		total := code + explain
		grp := VerbosityAggregateGroup{
			Key:           g.Key,
			CodeBytes:     code,
			ExplainBytes:  explain,
			TotalBytes:    total,
			TopLanguages:  topN(sortedLangBytes(b.CodeByLang()), verbosityAggregateMaxLangs),
			EstTotalUSD:   costByKey[g.Key],
			CostEstimated: pricedKey[g.Key],
		}
		if total > 0 {
			grp.CodePct = 100 * float64(code) / float64(total)
			grp.ExplainPct = 100 * float64(explain) / float64(total)
		}
		if explain > 0 {
			ratio := float64(code) / float64(explain)
			grp.CodeExplainRatio = &ratio
		}
		resp.Groups = append(resp.Groups, grp)
	}

	// Sort by est $ desc (where priced), then code bytes desc — the operator's
	// "what costs most / is most code-heavy" reading.
	sort.Slice(resp.Groups, func(i, j int) bool {
		if resp.Groups[i].EstTotalUSD != resp.Groups[j].EstTotalUSD {
			return resp.Groups[i].EstTotalUSD > resp.Groups[j].EstTotalUSD
		}
		return resp.Groups[i].CodeBytes > resp.Groups[j].CodeBytes
	})

	writeJSON(w, resp)
}

// topN returns the first n elements of a slice (or all when shorter).
func topN(ls []langBytes, n int) []langBytes {
	if len(ls) > n {
		return ls[:n]
	}
	return ls
}
