package codexanalytics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Surface selects which OpenAI analytics API the poller targets. A tenant may
// run a poller per surface; codex_analytics_daily.surface keeps them distinct.
type Surface string

const (
	// SurfaceChatGPTEnterprise is the ChatGPT-Enterprise Codex analytics API
	// (api.chatgpt.com, workspace-scoped, cost in credits).
	SurfaceChatGPTEnterprise Surface = "chatgpt_enterprise"
	// SurfaceOpenAIOrg is the OpenAI org Usage + Cost API (api.openai.com,
	// org-scoped, cost in dollars).
	SurfaceOpenAIOrg Surface = "openai_org"
)

// Unit is the measurement unit of a metric's value. THE cross-vendor unit trap:
// cost is reported in three different units across analytics surfaces, so it is
// recorded per row and normalized to USD only at the spendCTE merge.
type Unit string

const (
	UnitCredits Unit = "credits" // ChatGPT-Enterprise cost
	UnitUSD     Unit = "usd"     // OpenAI-org cost
	UnitTokens  Unit = "tokens"  // token counts
	UnitCount   Unit = "count"   // threads / turns / sessions / model_requests
)

// Actor types stored in actor_type.
const (
	ActorUser       = "user"       // a human developer (email or workspace user id)
	ActorAutomation = "automation" // CI / service / api-key usage
	ActorWorkspace  = "workspace"  // workspace-aggregate row (no per-user attribution)
)

// Metric names stored in codex_analytics_daily. Per-model breakdowns are summed
// into user-day totals — the grain the spend merge + overview need. Codex's
// analytics exposes neither acceptance rate nor lines-of-code (unlike CC).
const (
	MetricCost          = "cost"           // unit: credits | usd
	MetricTokensInput   = "tokens_input"   // unit: tokens
	MetricTokensOutput  = "tokens_output"  // unit: tokens
	MetricTokensCached  = "tokens_cached"  // unit: tokens (cache-read analogue)
	MetricThreads       = "threads"        // unit: count (chatgpt surface)
	MetricTurns         = "turns"          // unit: count (chatgpt surface)
	MetricModelRequests = "model_requests" // unit: count (openai-org surface)
)

// window is a closed-open time range [Start, End) the surfaces format per their
// own timestamp convention (ISO 8601 vs Unix seconds).
type window struct {
	Start time.Time
	End   time.Time
}

// DailyMetric is one normalized (day, user, surface, metric) value bound for
// codex_analytics_daily. Surface + Unit are stamped by the surface parser.
type DailyMetric struct {
	Day       string  // YYYY-MM-DD (UTC)
	UserKey   string  // email | workspace user id | OpenAI user_id
	ActorType string  // ActorUser | ActorAutomation | ActorWorkspace
	Surface   Surface // which API produced this row
	Unit      Unit    // unit of Value
	Metric    string
	Value     float64
}

// surfaceSpec is the resolved-once strategy for one surface. The poller is
// surface-blind: it only calls poll, which owns the surface's endpoint topology
// (ChatGPT = one endpoint; OpenAI-org = usage + costs) over the shared paginate
// helper. Selecting the spec at construction (not per-call) keeps the hot path
// free of surface conditionals.
type surfaceSpec struct {
	surface Surface
	baseURL string
	poll    func(ctx context.Context, p *Poller, win window) ([]DailyMetric, error)
}

// surfaceRegistry is the table-driven set of supported surfaces (rule #5: a data
// table, not a conditional ladder). Adding a surface is one entry + its file.
var surfaceRegistry = map[Surface]surfaceSpec{
	SurfaceChatGPTEnterprise: {
		surface: SurfaceChatGPTEnterprise,
		baseURL: "https://api.chatgpt.com",
		poll:    pollChatGPTEnterprise,
	},
	SurfaceOpenAIOrg: {
		surface: SurfaceOpenAIOrg,
		baseURL: "https://api.openai.com",
		poll:    pollOpenAIOrg,
	},
}

// resolveSurface returns the spec for a surface name (with an optional baseURL
// override for testing), or an error for an unknown surface.
func resolveSurface(name string, baseURLOverride string) (surfaceSpec, error) {
	spec, ok := surfaceRegistry[Surface(strings.TrimSpace(name))]
	if !ok {
		return surfaceSpec{}, fmt.Errorf("codexanalytics: unknown surface %q (want %s|%s)",
			name, SurfaceChatGPTEnterprise, SurfaceOpenAIOrg)
	}
	if o := strings.TrimSpace(baseURLOverride); o != "" {
		spec.baseURL = o
	}
	return spec, nil
}

// dayOf returns the YYYY-MM-DD UTC bucket for a metric. RFC3339 / date-only /
// Unix-seconds inputs are all accepted (the surfaces normalize timestamps).
func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// emitMetric is a small helper the surface parsers use to build a DailyMetric.
func emitMetric(day, userKey, actorType string, surface Surface, unit Unit, metric string, v float64) DailyMetric {
	return DailyMetric{
		Day: day, UserKey: userKey, ActorType: actorType,
		Surface: surface, Unit: unit, Metric: metric, Value: v,
	}
}

// newGet builds an authenticated GET with the surface's Bearer auth + a
// User-Agent. The one place per request the secret is attached.
func newGet(ctx context.Context, rawURL, apiKey string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("codexanalytics: new request: %w", err)
	}
	hk, hv := authHeader(apiKey)
	req.Header.Set(hk, hv)
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}
