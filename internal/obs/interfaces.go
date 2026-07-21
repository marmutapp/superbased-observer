package obs

import (
	"context"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

// This file declares the host-facing dependency surface from obs: the three
// interfaces obs defines and the host implements at the single wiring point
// (docs/plans/generalized-observability-custom-app-plan-2026-06-27.md §2.3):
// TurnSink, ProxyEnricher, ContentGate.
//
// P5 adds a FOURTH host interface — eval.JudgeClient (plan §8/§11) — for the
// LLM-as-judge eval scorer's outbound model call. It is defined in the pure
// internal/obs/eval package, NOT here, solely to avoid an import cycle (the
// obs-root EvalRunner imports eval, so eval cannot import obs root). It is
// still "defined by obs, implemented by the host" — the same contract — just
// in the eval subpackage. The judge call is the only outbound network in the
// whole subsystem, made only for an explicitly-invoked eval run; when no judge
// is wired, code scorers run fully offline. The host never imports obs except
// to bind these at wiring; obs never imports the host's proxy/store/routing
// internals.

// LLMTurnFacts is the minimal, content-free set an LLM span hands the host so
// the host can reconcile it into api_turns THROUGH the existing turnmerge
// seam. The host maps Source → turnmerge.Fidelity in its one existing place
// (internal/store/merge.go::fidelityForSource); obs never ranks fidelity.
type LLMTurnFacts struct {
	RequestID    string // the dedup key; reconciliation is a no-op when empty
	Source       span.Source
	Provider     string
	Model        string
	InputTokens  *int64
	OutputTokens *int64
	CostUSD      *float64
}

// TurnSink reconciles an LLM span's exact cost into api_turns via the host's
// existing upsert + turnmerge. obs NEVER writes api_turns directly — the
// coupling is this one removable interface (rule #4: api_turns keeps its
// owner). Implementations must be safe to call with an empty RequestID (they
// should no-op the reconciliation, since there is nothing to merge on).
type TurnSink interface {
	ReconcileLLMSpan(ctx context.Context, facts LLMTurnFacts) error
}

// Enrichment is the pull-only bundle obs renders ON a span the proxy also
// saw — Observer's wedge made visible (§9). All fields are best-effort; a
// missing source leaves the zero value.
type Enrichment struct {
	Found               bool    `json:"found"`
	Provider            string  `json:"provider"`
	Model               string  `json:"model"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	RoutingReason       string  `json:"routing_reason"` // why this model (routing decision), if any
	GuardVerdict        string  `json:"guard_verdict"`  // inline guard verdict at the proxy edge, if any
}

// ProxyEnricher is the read-only PULL interface (§2.3/§9): obs ASKS the host
// for facts about a request_id; the host (proxy/cachetrack/routing/guard)
// never calls into obs and never hands it their types. Removing obs removes
// the enrichment with zero change to those packages.
type ProxyEnricher interface {
	EnrichByRequestID(ctx context.Context, requestID string) (Enrichment, error)
}

// SpanCostFacts is the content-free set an LLM/embedding span hands the host
// so it can PRICE the span when the instrumentor reported no cost. Token fields
// are nullable (a nil pointer means "not reported"). The host owns the gross→net
// input convention (Anthropic input is already net; OpenAI/Gemini are gross and
// must net against CacheReadTokens) and the cache-tier rates — obs never imports
// the cost engine (rule #4: the coupling is this one removable interface).
type SpanCostFacts struct {
	Model            string
	Provider         string
	Source           span.Source
	InputTokens      *int64
	OutputTokens     *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	ReasoningTokens  *int64
}

// SpanCost is the host's pricing answer. Found is false when the model is
// unknown to the cost table (→ obs leaves the span unpriced, an honest gap).
// The component fields mirror obs's CostBreakdown; the host avoids any double
// billing (it does NOT add reasoning on top of output) per the operator's
// double-bill rule for the compute path.
type SpanCost struct {
	Found         bool
	TotalUSD      float64
	InputUSD      float64
	OutputUSD     float64
	CacheReadUSD  float64
	CacheWriteUSD float64
}

// SpanPricer prices a span THROUGH the host cost engine when no cost was
// reported (Gap B). Optional: a nil pricer simply leaves raw-agent spans
// unpriced. obs calls it only as a fallback — a reported cost always wins.
type SpanPricer interface {
	PriceSpan(ctx context.Context, facts SpanCostFacts) (SpanCost, error)
}

// ContentGate is the existing node content posture
// (ShareOptions.shipsRawContent()), injected so obs honors the same raw-body
// rule without importing the org-push internals. When AllowsRawContent() is
// false, obs persists only content hashes + bounded excerpts, never raw
// bodies — the metadata-first default that keeps Observer air-gapped (§10).
type ContentGate interface {
	AllowsRawContent() bool
}
