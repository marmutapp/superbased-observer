// Package span holds the canonical span/trace data model for the
// observability subsystem. It is PURE: no database/sql, net/http, or
// fsnotify (pinned by imports_test.go). Both OTel GenAI (`gen_ai.*`) and
// Arize OpenInference (`llm.*`/`openinference.span.kind`) conventions are
// normalized INTO these types at the ingestion boundary (internal/obs/ingest,
// Phase 2); every downstream consumer reads only this canonical shape and
// never branches on the source convention (CLAUDE.md module rule #3).
package span

import "time"

// Kind is the canonical span-kind vocabulary. Instrumentor-specific kinds
// (OpenInference `openinference.span.kind`, GenAI operation names) map onto
// these at the boundary; no other enum is used downstream.
type Kind string

// The canonical span kinds.
const (
	KindLLM       Kind = "llm"       // a model inference call
	KindTool      Kind = "tool"      // a tool / function execution
	KindRetriever Kind = "retriever" // a retrieval / vector-search step
	KindEmbedding Kind = "embedding" // an embedding call
	KindChain     Kind = "chain"     // a composed step (LangChain "chain")
	KindAgent     Kind = "agent"     // an agent / planner span (often the root)
	KindGuardrail Kind = "guardrail" // a guard / policy check
	KindEvaluator Kind = "evaluator" // a scorer / judge span
	KindEvent     Kind = "event"     // a generic point-in-time span
)

// Source is the provenance tag stamped on every trace/span at ingestion. The
// HOST maps a Source to a turnmerge.Fidelity in the ONE existing place
// (internal/store/merge.go::fidelityForSource) when an LLM span reconciles
// into api_turns via TurnSink — obs never ranks fidelity itself.
type Source string

// The obs ingestion sources (additive rows in the host's source→fidelity
// table; both rank as Approx — SDK/instrumentor-reported tokens are estimates).
const (
	SourceSDKOTLP   Source = "sdk_otlp"   // via the SuperBased thin SDK
	SourceOTLPTrace Source = "otlp_trace" // via a raw third-party OTLP exporter
)

// Status is the canonical span/trace outcome.
type Status string

// The canonical statuses (OTel status_code maps onto these).
const (
	StatusUnset Status = "unset"
	StatusOK    Status = "ok"
	StatusError Status = "error"
)

// Trace is one root operation (a request, an agent run). trace_id is the
// provider/OTel trace id and is the PK within the obs_* set.
type Trace struct {
	TraceID     string
	SessionID   string // nullable; user-supplied session.id/thread.id/gen_ai.conversation.id
	ThreadID    string // nullable
	Tenant      string // nullable resource tag
	User        string // nullable resource tag
	Source      Source
	RootSpanID  string
	ProjectRoot string // gated like existing content (stored only when ContentGate allows)
	Status      Status
	StartedAt   time.Time
	EndedAt     time.Time // zero ⇒ still open
}

// Span is one node in a trace's tree. Token/cost columns are nullable and
// authoritative-on-merge: a span may carry approximate tokens that a proxy
// api_turn later supersedes (§5). A nil pointer means "not reported".
type Span struct {
	SpanID       string
	TraceID      string
	ParentSpanID string // nullable ⇒ root
	Kind         Kind
	Name         string
	Status       Status
	StartedAt    time.Time
	EndedAt      time.Time // zero ⇒ still open

	// LLM-span fields (all nullable / zero on non-llm spans).
	Model        string
	Provider     string
	InputTokens  *int64
	OutputTokens *int64
	TotalTokens  *int64
	// Token detail (nullable; populated only when the instrumentor emits the
	// discrete attribute — model-dependent, see the token-detail plan). Cache
	// write is Anthropic-only on the wire; OpenAI/Gemini report read-only.
	// Reasoning is reasoning-model-only (folded into output elsewhere).
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	ReasoningTokens  *int64
	// Cost (nullable). CostUSD is the total; CostSource records whether it was
	// reported by the instrumentor or computed by the host cost engine
	// (CostReported / CostComputed); CostDetail is the per-component breakdown.
	// CostUSD/CostDetail/CostSource are set together at the boundary (the
	// mapper for reported, the ingestor's SpanPricer fallback for computed).
	CostUSD            *float64
	CostSource         CostSource
	CostDetail         *CostBreakdown
	RequestID          string // soft join value to api_turns (NOT an FK), nullable
	ProviderResponseID string // secondary dedup key (§5.2)
	ToolCallID         string
	Source             Source
}

// CostSource records the provenance of a span's cost.
type CostSource string

// The cost-provenance values. Empty ⇒ no cost known (no model/tokens, nothing
// reported, no pricer).
const (
	CostReported CostSource = "reported" // emitted by the instrumentor (e.g. llm.cost.*)
	CostComputed CostSource = "computed" // priced by the host cost engine (SpanPricer)
)

// CostBreakdown is the per-component cost split for a span, in USD. Every field
// is nullable (a nil pointer means "that component is unknown/zero", distinct
// from a present 0). Stored as JSON in obs_spans.cost_detail; display-only — the
// hero/list aggregate sums the cost_usd total, not these.
type CostBreakdown struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
	Reasoning  *float64 `json:"reasoning,omitempty"`
	Tool       *float64 `json:"tool,omitempty"`
}

// Empty reports whether the breakdown carries no component at all.
func (c *CostBreakdown) Empty() bool {
	return c == nil || (c.Input == nil && c.Output == nil && c.CacheRead == nil &&
		c.CacheWrite == nil && c.Reasoning == nil && c.Tool == nil)
}

// SpanEvent is a timestamped event within a span (OTel span event:
// exception, tool-result marker, retry). AttributesJSON holds metadata only.
type SpanEvent struct { //nolint:revive // Span-prefixed names are intentional in the span data-model package
	SpanID         string
	Time           time.Time
	Name           string
	AttributesJSON string
}

// SpanLink is a cross-trace causal link (OTel link) for multi-agent handoffs
// / distributed propagation. Rendered as a jump affordance, not a graph edge.
type SpanLink struct { //nolint:revive // Span-prefixed names are intentional in the span data-model package
	SpanID         string
	LinkedTrace    string
	LinkedSpan     string
	AttributesJSON string
}

// ContentKind tags a stored body by role.
type ContentKind string

// The content roles persisted in obs_span_content.
const (
	ContentPrompt   ContentKind = "prompt"
	ContentResponse ContentKind = "response"
	ContentToolIO   ContentKind = "tool_io"
)

// SpanContent is a raw prompt/response/tool-io body, separated from obs_spans
// and gated by the injected ContentGate. ContentHash is ALWAYS set; Raw is
// stored only when the node's content posture allows it (§10). Node-local;
// never selected by the org-push seam.
type SpanContent struct { //nolint:revive // Span-prefixed names are intentional in the span data-model package
	SpanID      string
	TraceID     string
	RequestID   string
	Kind        ContentKind
	ContentHash string // always present
	Raw         string // empty unless ContentGate.AllowsRawContent()
	Time        time.Time
}
