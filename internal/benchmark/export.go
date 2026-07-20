package benchmark

import "encoding/json"

// export.go is the SINGLE canonical-JSON serialization owner (plan §4.3): the
// CLI `benchmark export`/`report --json` verbs AND the dashboard export
// endpoint share this one shape so surfaces can't disagree. It carries the
// qualifier tuple (model · workload · date · N · CI · "estimated list price")
// and EXCLUDES prompts, repo/workspace paths, final-answer excerpts, and judge
// rationale by default — a judge rationale can reproduce source or secrets
// (plan §3.12). It lives in the pure package (no SQL/HTTP) so both the CLI
// (package main) and the dashboard (package dashboard) can build it.

const (
	// ExportSchema versions the canonical results payload.
	ExportSchema = "superbased.benchmark.v1"
	// PriceDisclaimer is the inseparable cost qualifier stamped on every
	// exported / rendered cost figure.
	PriceDisclaimer = "estimated list price, not invoiced"
)

// Export is the canonical, redaction-allowlisted results payload.
type Export struct {
	Schema          string             `json:"schema"`
	RunID           string             `json:"run_id"`
	SpecName        string             `json:"spec_name"`
	SpecHash        string             `json:"spec_hash"`
	GeneratedAt     string             `json:"generated_at"`
	FinishedAt      string             `json:"finished_at,omitempty"`
	Status          string             `json:"status"`
	Baseline        string             `json:"baseline_config"`
	Margin          float64            `json:"noninferiority_margin"`
	PriceDisclaimer string             `json:"price_disclaimer"`
	TotalSpendUSD   float64            `json:"total_spend_usd"`
	Configs         []ExportConfig     `json:"configs"`
	Comparisons     []ExportComparison `json:"comparisons"`
	StatusCensus    map[string]int     `json:"status_census"`
	Warnings        []string           `json:"warnings,omitempty"`
	// Manifest + PricingSnapshot are content-free (models, versions, repo refs,
	// price table hash) and ride verbatim.
	Manifest        json.RawMessage `json:"manifest,omitempty"`
	PricingSnapshot json.RawMessage `json:"pricing_snapshot,omitempty"`
}

// ExportInterval is a point estimate + CI bounds.
type ExportInterval struct {
	Point float64 `json:"point"`
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
}

// ExportConfig is one matrix row's evidence, with N at every level.
type ExportConfig struct {
	ConfigID          string         `json:"config_id"`
	Harness           string         `json:"harness"`
	Model             string         `json:"model"`
	Planned           int            `json:"n_planned"`
	Executed          int            `json:"n_executed"`
	Scored            int            `json:"n_scored"`
	Passed            int            `json:"n_passed"`
	Sessions          int            `json:"n_sessions"`
	Tasks             int            `json:"n_tasks"`
	SuccessRate       float64        `json:"success_rate"`
	SuccessCI         ExportInterval `json:"success_ci"`
	ModelEligibleRate float64        `json:"model_eligible_rate"`
	ModelEligibleCI   ExportInterval `json:"model_eligible_ci"`
	TotalSpendUSD     float64        `json:"total_spend_usd"`
	MeanCostUSD       float64        `json:"mean_cost_per_attempt_usd"`
	SDCostUSD         float64        `json:"sd_cost_usd"`
	MedianCostUSD     float64        `json:"median_cost_usd"`
	IQRLoUSD          float64        `json:"iqr_lo_usd"`
	IQRHiUSD          float64        `json:"iqr_hi_usd"`
	CostPerSuccessUSD *float64       `json:"cost_per_success_usd"` // null = censored (no successes)
	MeanWallMS        float64        `json:"mean_wall_ms"`
	InputTokens       int64          `json:"input_tokens"`
	OutputTokens      int64          `json:"output_tokens"`
	CacheReadTokens   int64          `json:"cache_read_tokens"`
	CacheReadPct      float64        `json:"cache_read_pct"`
}

// ExportComparison is one candidate-vs-baseline verdict + evidence.
type ExportComparison struct {
	Candidate    string         `json:"candidate"`
	Baseline     string         `json:"baseline"`
	DiffCI       ExportInterval `json:"success_diff_ci"`
	PairedDelta  ExportInterval `json:"paired_delta"`
	PairedTasks  int            `json:"paired_tasks"`
	Cheaper      bool           `json:"cheaper"`
	CostDeltaUSD float64        `json:"cost_per_success_delta_usd"`
	Verdict      string         `json:"verdict"`
}

// BuildExport is the one serializer both the CLI export and the dashboard
// export endpoint use.
func BuildExport(run RunRecord, rep Report, generatedAt string) Export {
	exp := Export{
		Schema: ExportSchema, RunID: run.RunID, SpecName: rep.SpecName,
		SpecHash: run.SpecHash, GeneratedAt: generatedAt, Status: run.Status,
		Baseline: rep.Baseline, Margin: rep.Margin, PriceDisclaimer: PriceDisclaimer,
		TotalSpendUSD: run.SpendUSD, StatusCensus: CensusToStrings(rep.StatusCensus),
		Warnings: rep.Warnings,
	}
	if !run.FinishedAt.IsZero() {
		exp.FinishedAt = run.FinishedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if run.ManifestJSON != "" {
		exp.Manifest = json.RawMessage(run.ManifestJSON)
	}
	if run.PricingSnapshotJSON != "" {
		exp.PricingSnapshot = json.RawMessage(run.PricingSnapshotJSON)
	}
	for _, c := range rep.Configs {
		ec := ExportConfig{
			ConfigID: c.ConfigID, Harness: c.Harness, Model: c.Model,
			Planned: c.Planned, Executed: c.Executed, Scored: c.Scored, Passed: c.Passed,
			Sessions: c.Sessions, Tasks: c.Tasks,
			SuccessRate: c.SuccessRate, SuccessCI: exportIV(c.SuccessCI),
			ModelEligibleRate: c.ModelEligibleRate, ModelEligibleCI: exportIV(c.ModelEligibleCI),
			TotalSpendUSD: c.TotalSpendUSD, MeanCostUSD: c.MeanCostPerAttempt, SDCostUSD: c.SDCostUSD,
			MedianCostUSD: c.MedianCostUSD, IQRLoUSD: c.IQRLoUSD, IQRHiUSD: c.IQRHiUSD,
			MeanWallMS: c.MeanWallMS, InputTokens: c.InputTokens, OutputTokens: c.OutputTokens,
			CacheReadTokens: c.CacheReadTokens, CacheReadPct: c.CacheReadPct,
		}
		if c.CostPerSuccessDefined {
			v := c.CostPerSuccessUSD
			ec.CostPerSuccessUSD = &v
		}
		exp.Configs = append(exp.Configs, ec)
	}
	for _, cmp := range rep.Comparisons {
		exp.Comparisons = append(exp.Comparisons, ExportComparison{
			Candidate: cmp.Candidate, Baseline: cmp.Baseline,
			DiffCI: exportIV(cmp.DiffCI), PairedDelta: exportIV(cmp.PairedDelta), PairedTasks: cmp.PairedTasks,
			Cheaper: cmp.Cheaper, CostDeltaUSD: cmp.CostDeltaUSD, Verdict: string(cmp.Verdict),
		})
	}
	return exp
}

func exportIV(i Interval) ExportInterval {
	return ExportInterval{Point: i.Point, Lo: i.Lo, Hi: i.Hi}
}

// CensusToStrings converts a Status-keyed census to string keys for JSON.
func CensusToStrings(m map[Status]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}
