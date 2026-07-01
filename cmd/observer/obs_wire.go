//go:build !no_obs

package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	otlpingest "github.com/marmutapp/superbased-observer/internal/ingest/otlp"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/eval"
	"github.com/marmutapp/superbased-observer/internal/obs/httpapi"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// obsDashboardRoutes returns the obs trajectory endpoints (/api/obs/*) for the
// host dashboard's shared mux when [observability] is enabled, or nil
// otherwise (decision D4 — the dashboard never imports obs; it just receives
// generic routes here, the single host->obs seam). Build-tagged; the no_obs
// build returns nil.
func obsDashboardRoutes(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger) []dashboard.ExtraRoute {
	if !cfg.Observability.Enabled {
		return nil
	}
	obsStore, err := obsstore.Open(ctx, db)
	if err != nil {
		logger.Warn("observability: dashboard routes disabled — schema init failed", "err", err)
		return nil
	}
	api := httpapi.New(obsStore, obsProxyEnricher{st: store.New(db)}, logger)
	var routes []dashboard.ExtraRoute
	for _, r := range api.Routes() {
		routes = append(routes, dashboard.ExtraRoute{Pattern: r.Pattern, Handler: r.Handler})
	}
	return routes
}

// obsOrgProviders binds the org-tier observability provider seam (obs-org-tier
// plan §2) over the obs-owned reads in internal/obs/store, returning a plain
// store.ObsOrgProviders (func fields over orgcontract — no obs types leak). The
// host's buildOrgBundle calls this and SetObsOrgProviders on the push store, so
// the push path composes the opt-in obs tiers without internal/store importing
// internal/obs. Returns the zero value (every provider nil → every tier a
// no-op) when [observability] is disabled or the schema fails to open; the
// no_obs build returns the zero value via the stub.
func obsOrgProviders(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger) store.ObsOrgProviders {
	if !cfg.Observability.Enabled {
		return store.ObsOrgProviders{}
	}
	obsStore, err := obsstore.Open(ctx, db)
	if err != nil {
		logger.Warn("observability: org providers disabled — schema init failed", "err", err)
		return store.ObsOrgProviders{}
	}
	return store.ObsOrgProviders{
		Summaries: obsStore.AggregateForOrg,
		Spans:     obsStore.SpansForOrg,
		Content:   obsStore.ContentForOrg,
		EvalRuns:  obsStore.EvalRunsForOrg,
	}
}

// newObsTraceHandler is the SINGLE place the host imports internal/obs (the
// reverse-import invariant in tests/invariant/obs_boundary_test.go allows only
// this file; the no_obs build replaces it with the stub, compiling the
// subsystem out). When [observability] is enabled it opens the obs-owned
// schema (decision D3 — applied only here, so a disabled node creates no
// obs_* tables), wires the host's api_turns reconciliation as the TurnSink,
// and returns the generic trace handler for the shared OTLP receiver. Returns
// nil when disabled, so /v1/traces is simply not served.
func newObsTraceHandler(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger) otlpingest.TraceHandler {
	if !cfg.Observability.Enabled {
		return nil
	}
	obsStore, err := obsstore.Open(ctx, db)
	if err != nil {
		logger.Warn("observability: schema init failed — trace ingestion disabled", "err", err)
		return nil
	}
	ingestor := obs.NewTraceIngestor(obsStore, obsTurnSink{st: store.New(db)}, logger)
	ingestor.SetContentGate(obsContentGateFor(cfg))
	ingestor.SetSpanPricer(obsSpanPricer{engine: cost.NewEngine(cfg.Intelligence)})
	if sampler := buildObsOnlineSampler(cfg, obsStore, logger); sampler != nil {
		ingestor.SetSampler(sampler)
		logger.Info("observability: online eval sampling enabled", "rate", cfg.Observability.Eval.OnlineSampleRate)
	}
	logger.Info("observability: enabled (obs schema ready; /v1/traces serving)")
	return ingestor.Ingest
}

// buildObsOnlineSampler constructs the online eval sampler from
// [observability.eval] (plan §8), or returns nil when online sampling is off
// or its scorers don't parse/build. The judge is nil — online sampling runs
// only facts-based code scorers.
func buildObsOnlineSampler(cfg config.Config, obsStore *obsstore.Store, logger *slog.Logger) *obs.OnlineSampler {
	ec := cfg.Observability.Eval
	if ec.OnlineSampleRate <= 0 || len(ec.OnlineScorers) == 0 {
		return nil
	}
	specs, err := eval.ParseSpecs(ec.OnlineScorers)
	if err != nil {
		logger.Warn("observability: online_scorers parse failed — online sampling off", "err", err)
		return nil
	}
	scorers, err := eval.BuildAll(specs, nil)
	if err != nil {
		logger.Warn("observability: online_scorers build failed — online sampling off", "err", err)
		return nil
	}
	return obs.NewOnlineSampler(obsStore, scorers, ec.OnlineSampleRate, logger)
}

// obsTurnSink implements obs.TurnSink over the host's existing
// store.UpsertTurnByRequestID + turnmerge. obs's source string maps to
// FidelityApprox in the ONE host place (store.fidelityForSource), so a proxy
// or native-OTel turn for the same request_id always wins on token/cost.
type obsTurnSink struct {
	st *store.Store
}

func (s obsTurnSink) ReconcileLLMSpan(ctx context.Context, facts obs.LLMTurnFacts) error {
	if facts.RequestID == "" {
		return nil // nothing to merge on
	}
	t := models.APITurn{
		RequestID: facts.RequestID,
		Source:    string(facts.Source),
		Provider:  facts.Provider,
		Model:     facts.Model,
		Timestamp: time.Now().UTC(),
	}
	if facts.InputTokens != nil {
		t.InputTokens = *facts.InputTokens
	}
	if facts.OutputTokens != nil {
		t.OutputTokens = *facts.OutputTokens
	}
	if facts.CostUSD != nil {
		t.CostUSD = *facts.CostUSD
	}
	_, _, err := s.st.UpsertTurnByRequestID(ctx, t)
	return err
}

// obsProxyEnricher implements obs.ProxyEnricher (§9 / P6) over the host's
// existing read seam store.EnrichmentByRequestID. It is strictly PULL-only:
// obs asks the host for facts about a request_id; the proxy/cachetrack/routing/
// guard packages never call into obs and never hand it their types. Removing
// obs removes the enrichment with zero change to those packages. GuardVerdict
// is now populated — the proxy response-inspection path anchors
// guard_events.api_turn_id, so a verdict for the turn is joinable (empty when
// the guard flagged nothing).
type obsProxyEnricher struct {
	st *store.Store
}

func (e obsProxyEnricher) EnrichByRequestID(ctx context.Context, requestID string) (obs.Enrichment, error) {
	re, found, err := e.st.EnrichmentByRequestID(ctx, requestID)
	if err != nil || !found {
		return obs.Enrichment{}, err
	}
	return obs.Enrichment{
		Found:               true,
		Provider:            re.Provider,
		Model:               re.Model,
		InputTokens:         re.InputTokens,
		OutputTokens:        re.OutputTokens,
		CacheReadTokens:     re.CacheReadTokens,
		CacheCreationTokens: re.CacheCreationTokens,
		CostUSD:             re.CostUSD,
		RoutingReason:       re.RoutingReason,
		GuardVerdict:        re.GuardVerdict,
	}, nil
}

// obsContentGate implements obs.ContentGate over the node's existing
// full-content posture (the same predicate as store.ShareOptions.shipsRawContent:
// FullContent || AdminManaged). obs honors it for raw-body persistence (plan
// §10) — e.g. eval datasets snapshot raw input/output only when this is true.
type obsContentGate struct{ allow bool }

func (g obsContentGate) AllowsRawContent() bool { return g.allow }

func obsContentGateFor(cfg config.Config) obsContentGate {
	return obsContentGate{allow: cfg.OrgClient.Share.FullContent || cfg.OrgClient.Share.AdminManaged}
}

// obsSpanPricer implements obs.SpanPricer over the host cost engine (Gap B). It
// is invoked ONLY for spans the instrumentor left unpriced; a reported cost
// always wins. This is where the gross→net input convention and the no-double-
// bill rule live (obs never imports internal/intelligence/cost — rule #4).
type obsSpanPricer struct{ engine *cost.Engine }

func (p obsSpanPricer) PriceSpan(_ context.Context, facts obs.SpanCostFacts) (obs.SpanCost, error) {
	if p.engine == nil || facts.Model == "" {
		return obs.SpanCost{}, nil
	}
	b := cost.TokenBundle{
		Output:        derefInt64(facts.OutputTokens),
		CacheRead:     derefInt64(facts.CacheReadTokens),
		CacheCreation: derefInt64(facts.CacheWriteTokens),
	}
	// Input netting. Anthropic reports input_tokens NET of cache already;
	// OpenAI/Gemini report it GROSS (prompt incl. cached) and must net against
	// cache-read or the cached portion bills at BOTH the input AND cache-read
	// rate (~3.4× overbill on cached turns — the double-bill the operator
	// flagged). Resolve the convention from provider family at the boundary
	// (capability, not identity). Reasoning is intentionally NOT added: both
	// providers fold thinking into the output count we already price, so adding
	// it again would double-bill.
	in := derefInt64(facts.InputTokens)
	if inputIsGross(facts.Provider) {
		in -= b.CacheRead
		if in < 0 {
			in = 0
		}
	}
	b.Input = in
	bd, ok := p.engine.ComputeBreakdown(facts.Model, b)
	if !ok {
		return obs.SpanCost{Found: false}, nil
	}
	return obs.SpanCost{
		Found:         true,
		TotalUSD:      bd.Total,
		InputUSD:      bd.InputCost,
		OutputUSD:     bd.OutputCost,
		CacheReadUSD:  bd.CacheReadCost,
		CacheWriteUSD: bd.CacheCreationCost,
	}, nil
}

// inputIsGross reports whether a provider's input/prompt token count INCLUDES
// the cached portion (so it must be netted against cache-read before pricing).
// Anthropic is net; OpenAI/Gemini/OpenRouter/etc are gross. An unknown/empty
// provider defaults to NET (no subtraction): a wrong subtraction silently
// undercounts every cached turn, whereas the rare unlabeled-gross span only
// slightly overstates an already-"estimated" cost.
func inputIsGross(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case p == "":
		return false
	case strings.Contains(p, "anthropic"), strings.Contains(p, "claude"):
		return false
	default:
		return true
	}
}

// derefInt64 returns the pointed-to value or 0 for a nil pointer.
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// --- eval plane host wrappers (plan §8) ------------------------------------
//
// These are the ONLY entry points the `observer eval` command calls; they keep
// the boundary intact (the cobra command file does not import internal/obs).
// They return PLAIN main-package types so no obs/eval/store type leaks past
// the seam (rule #2). The judge is nil — the host JudgeClient binding (the one
// outbound network) is deferred, so llm_judge errors clearly until it ships.

// obsDatasetInfo / obsRunInfo / obsEvalSummary are the plain shapes the CLI
// renders.
type obsDatasetInfo struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   string
	ItemCount   int64
}

type obsRunInfo struct {
	ID        int64
	Name      string
	StartedAt string
	Total     int
	Passed    int
	MeanScore float64
	Status    string
}

type obsEvalSummary struct {
	RunID     int64
	Total     int
	Passed    int
	MeanScore float64
	PassRate  float64
}

// obsEvalEnabled reports whether [observability] is on (the eval CLI needs it).
func obsEvalEnabled(cfg config.Config) bool { return cfg.Observability.Enabled }

// obsEvalScorerNames lists the built-in scorer names for CLI discovery.
func obsEvalScorerNames() []string { return eval.Names() }

func obsEvalRunner(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger) (*obs.EvalRunner, error) {
	obsStore, err := obsstore.Open(ctx, db)
	if err != nil {
		return nil, err
	}
	// The judge is bound only when [observability.eval] judge_model is set;
	// otherwise nil → llm_judge errors clearly and code scorers run offline.
	return obs.NewEvalRunner(obsStore, obsBuildJudge(cfg), obsContentGateFor(cfg), logger), nil
}

// obsJudgeClient adapts the generic chatCompletionsJudge to eval.JudgeClient.
// It defaults the model to [observability.eval] judge_model when a scorer spec
// omits its own model= param.
type obsJudgeClient struct {
	client       chatCompletionsJudge
	defaultModel string
}

func (j obsJudgeClient) Judge(ctx context.Context, req eval.JudgeRequest) (eval.JudgeResponse, error) {
	model := req.Model
	if model == "" {
		model = j.defaultModel
	}
	text, err := j.client.complete(ctx, model, req.Prompt)
	if err != nil {
		return eval.JudgeResponse{}, err
	}
	return eval.JudgeResponse{Text: text}, nil
}

// obsBuildJudge returns the host LLM-judge client, or nil when no judge model
// is configured (judge disabled). The credential is read from the env var
// named by [observability.eval] judge_api_key_env (default OPENROUTER_API_KEY)
// — never from config/disk. This is the ONLY place the host binds the outbound
// judge call; the daemon/online-sampling path always passes nil, so it stays
// network-free.
func obsBuildJudge(cfg config.Config) eval.JudgeClient {
	ec := cfg.Observability.Eval
	if ec.JudgeModel == "" {
		return nil
	}
	baseURL := ec.JudgeBaseURL
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	keyEnv := ec.JudgeAPIKeyEnv
	if keyEnv == "" {
		keyEnv = "OPENROUTER_API_KEY"
	}
	return obsJudgeClient{
		client:       newChatCompletionsJudge(baseURL, os.Getenv(keyEnv)),
		defaultModel: ec.JudgeModel,
	}
}

func obsEvalCreateDatasetFromTraces(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger, name, desc string, limit int) (int64, int, error) {
	r, err := obsEvalRunner(ctx, cfg, db, logger)
	if err != nil {
		return 0, 0, err
	}
	return r.CreateDatasetFromTraces(ctx, name, desc, limit)
}

func obsEvalListDatasets(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger) ([]obsDatasetInfo, error) {
	r, err := obsEvalRunner(ctx, cfg, db, logger)
	if err != nil {
		return nil, err
	}
	rows, err := r.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]obsDatasetInfo, 0, len(rows))
	for _, d := range rows {
		out = append(out, obsDatasetInfo{ID: d.ID, Name: d.Name, Description: d.Description, CreatedAt: d.CreatedAt, ItemCount: d.ItemCount})
	}
	return out, nil
}

func obsEvalRun(ctx context.Context, cfg config.Config, db *sql.DB, logger *slog.Logger, datasetName string, scorerSpecs []string, runName, judgePrompt, judgeModel string, judgeThreshold float64) (obsEvalSummary, error) {
	specs, err := eval.ParseSpecs(scorerSpecs)
	if err != nil {
		return obsEvalSummary{}, err
	}
	// Out-of-band llm_judge params from flags (so a prompt with commas isn't
	// shredded by the key=val,key2=val spec syntax). A param set in the spec
	// still wins over the flag.
	for i := range specs {
		if specs[i].Name != "llm_judge" {
			continue
		}
		if specs[i].Params == nil {
			specs[i].Params = map[string]string{}
		}
		if _, ok := specs[i].Params["prompt"]; !ok && judgePrompt != "" {
			specs[i].Params["prompt"] = judgePrompt
		}
		if _, ok := specs[i].Params["model"]; !ok && judgeModel != "" {
			specs[i].Params["model"] = judgeModel
		}
		if _, ok := specs[i].Params["threshold"]; !ok && judgeThreshold > 0 {
			specs[i].Params["threshold"] = strconv.FormatFloat(judgeThreshold, 'g', -1, 64)
		}
	}
	r, err := obsEvalRunner(ctx, cfg, db, logger)
	if err != nil {
		return obsEvalSummary{}, err
	}
	res, err := r.RunEval(ctx, datasetName, specs, runName)
	if err != nil {
		return obsEvalSummary{}, err
	}
	return obsEvalSummary{RunID: res.RunID, Total: res.Total, Passed: res.Passed, MeanScore: res.MeanScore, PassRate: res.PassRate}, nil
}
