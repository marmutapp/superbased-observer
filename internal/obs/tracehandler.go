package obs

import (
	"context"
	"log/slog"

	"github.com/marmutapp/superbased-observer/internal/obs/ingest"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// TraceIngestor is the obs-side glue between the generic OTLP trace receiver
// (internal/ingest/otlp, schema-blind) and the subsystem: map → persist the
// canonical span graph → reconcile each LLM span's cost into api_turns through
// the injected TurnSink. It is the single seam the wiring point hands the
// receiver as its TraceHandler.
type TraceIngestor struct {
	store   *obsstore.Store
	sink    TurnSink    // optional; nil disables api_turns reconciliation
	sampler SpanSampler // optional; nil disables online eval sampling
	gate    ContentGate // optional; nil ⇒ raw bodies stripped (hash-only)
	pricer  SpanPricer  // optional; nil ⇒ unreported-cost spans stay unpriced
	logger  *slog.Logger
}

// SpanSampler scores a freshly-ingested span (online eval, plan §8). The
// ingestor calls it without importing internal/obs/eval — the implementation
// (obs.OnlineSampler) holds the scorers. Defined here so the seam stays a
// plain interface; nil disables online sampling.
type SpanSampler interface {
	SampleSpan(ctx context.Context, s span.Span)
}

// NewTraceIngestor builds the ingestor. sink may be nil (spans still persist;
// no api_turns reconciliation happens).
func NewTraceIngestor(store *obsstore.Store, sink TurnSink, logger *slog.Logger) *TraceIngestor {
	if logger == nil {
		logger = slog.Default()
	}
	return &TraceIngestor{store: store, sink: sink, logger: logger}
}

// SetSampler attaches an online eval sampler (plan §8). Optional and additive —
// the no-sampler ingestor is unchanged. Pass nil to disable.
func (ti *TraceIngestor) SetSampler(s SpanSampler) { ti.sampler = s }

// SetContentGate attaches the node's content posture (§10) so captured prompt/
// response/tool-io bodies persist raw only when the node allows it; otherwise
// only their content hashes survive. Optional and additive — a nil gate (the
// default) strips every raw body, the metadata-first air-gapped default.
func (ti *TraceIngestor) SetContentGate(g ContentGate) { ti.gate = g }

// SetSpanPricer attaches the host cost engine (Gap B) so spans whose
// instrumentor reported NO cost get priced through it. Optional and additive —
// a nil pricer leaves those spans unpriced (an honest gap, not a fabricated $0).
// A reported cost is never overwritten.
func (ti *TraceIngestor) SetSpanPricer(p SpanPricer) { ti.pricer = p }

// Ingest maps one OTLP trace export and persists it. Best-effort and
// at-least-once: a per-stage error is logged and the rest proceeds, mirroring
// the logs handler — telemetry ingest never fails the export over one bad row.
// It returns nil so the receiver maps it to an OTLP success.
func (ti *TraceIngestor) Ingest(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) error {
	// The /v1/traces endpoint sources everything as otlp_trace; the thin SDK
	// sets the same echo-guard tag and is fidelity-equivalent (both Approx),
	// so a separate sdk_otlp tag is cosmetic and deferred.
	res := ingest.Map(req, span.SourceOTLPTrace)

	// Price spans whose instrumentor reported NO cost (Gap B fallback). A
	// reported cost (set by the mapper) always wins; this only fills the gap.
	if ti.pricer != nil {
		for i := range res.Spans {
			ti.priceSpan(ctx, &res.Spans[i])
		}
	}

	for i := range res.Traces {
		if err := ti.store.UpsertTrace(ctx, res.Traces[i]); err != nil {
			ti.logger.Warn("obs ingest: trace upsert failed", "trace_id", res.Traces[i].TraceID, "err", err)
		}
	}
	if err := ti.store.UpsertSpansBatch(ctx, res.Spans); err != nil {
		ti.logger.Warn("obs ingest: spans upsert failed", "count", len(res.Spans), "err", err)
	}
	if err := ti.store.UpsertSpanEvents(ctx, res.Events); err != nil {
		ti.logger.Warn("obs ingest: events upsert failed", "count", len(res.Events), "err", err)
	}
	if err := ti.store.UpsertSpanLinks(ctx, res.Links); err != nil {
		ti.logger.Warn("obs ingest: links upsert failed", "count", len(res.Links), "err", err)
	}
	if len(res.Content) > 0 {
		// Apply the node content posture HERE (the mapper stays pure): when the
		// gate denies raw bodies, strip them and keep only the hashes (§10).
		if ti.gate == nil || !ti.gate.AllowsRawContent() {
			for i := range res.Content {
				res.Content[i].Raw = ""
			}
		}
		if err := ti.store.InsertSpanContent(ctx, res.Content); err != nil {
			ti.logger.Warn("obs ingest: content insert failed", "count", len(res.Content), "err", err)
		}
	}

	if ti.sink != nil {
		for i := range res.Spans {
			ti.reconcile(ctx, res.Spans[i])
		}
	}
	if ti.sampler != nil {
		for i := range res.Spans {
			ti.sampler.SampleSpan(ctx, res.Spans[i])
		}
	}
	return nil
}

// priceSpan fills a span's cost from the host cost engine when the instrumentor
// reported none. It runs only for spans with a model + at least one token count
// (LLM / embedding / retriever spans — tool spans carry no model, so they stay
// unpriced, which is correct). A reported cost short-circuits. The host owns the
// gross→net + double-bill rules; obs just records the answer + a "computed"
// provenance. Fail-open: a pricer error logs and leaves the span unpriced.
func (ti *TraceIngestor) priceSpan(ctx context.Context, s *span.Span) {
	if s.CostSource == span.CostReported || s.CostUSD != nil || s.Model == "" {
		return
	}
	if s.InputTokens == nil && s.OutputTokens == nil && s.CacheReadTokens == nil &&
		s.CacheWriteTokens == nil {
		return // no priceable tokens
	}
	c, err := ti.pricer.PriceSpan(ctx, SpanCostFacts{
		Model:            s.Model,
		Provider:         s.Provider,
		Source:           s.Source,
		InputTokens:      s.InputTokens,
		OutputTokens:     s.OutputTokens,
		CacheReadTokens:  s.CacheReadTokens,
		CacheWriteTokens: s.CacheWriteTokens,
		ReasoningTokens:  s.ReasoningTokens,
	})
	if err != nil {
		ti.logger.Warn("obs ingest: span price failed", "span_id", s.SpanID, "model", s.Model, "err", err)
		return
	}
	if !c.Found {
		return // unknown model — leave unpriced, never a fabricated $0
	}
	total := c.TotalUSD
	s.CostUSD = &total
	s.CostSource = span.CostComputed
	s.CostDetail = nonZeroBreakdown(c)
}

// nonZeroBreakdown maps a computed SpanCost to a CostBreakdown, including only
// the components the engine actually charged (a 0 component is left nil so the
// drawer doesn't render empty $0.00 rows). Reasoning is intentionally absent —
// the compute path folds it into output to avoid double billing.
func nonZeroBreakdown(c SpanCost) *span.CostBreakdown {
	bd := &span.CostBreakdown{}
	if c.InputUSD != 0 {
		bd.Input = &c.InputUSD
	}
	if c.OutputUSD != 0 {
		bd.Output = &c.OutputUSD
	}
	if c.CacheReadUSD != 0 {
		bd.CacheRead = &c.CacheReadUSD
	}
	if c.CacheWriteUSD != 0 {
		bd.CacheWrite = &c.CacheWriteUSD
	}
	if bd.Empty() {
		return nil
	}
	return bd
}

// reconcile feeds an LLM span carrying a request_id to the host's TurnSink so
// its (approximate) cost dedups against any proxy turn for the same id. Non-
// LLM spans and spans without a join key are skipped (they can't collide with
// a proxy turn). One bad reconcile logs and never blocks the batch.
func (ti *TraceIngestor) reconcile(ctx context.Context, s span.Span) {
	if s.Kind != span.KindLLM || s.RequestID == "" {
		return
	}
	if err := ti.sink.ReconcileLLMSpan(ctx, LLMTurnFacts{
		RequestID:    s.RequestID,
		Source:       s.Source,
		Provider:     s.Provider,
		Model:        s.Model,
		InputTokens:  s.InputTokens,
		OutputTokens: s.OutputTokens,
		CostUSD:      s.CostUSD,
	}); err != nil {
		ti.logger.Warn("obs ingest: turn reconcile failed", "request_id", s.RequestID, "err", err)
	}
}
