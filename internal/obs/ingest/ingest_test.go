package ingest

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func kvStr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func export(res []*commonpb.KeyValue, spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   &resourcepb.Resource{Attributes: res},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		}},
	}
}

func bySpanID(spans []span.Span) map[string]span.Span {
	m := map[string]span.Span{}
	for _, s := range spans {
		m[s.SpanID] = s
	}
	return m
}

// TestMap_GenAIConvention maps an OTel GenAI llm span: provider/model/tokens
// from gen_ai.* keys; request_id falls back to gen_ai.response.id.
func TestMap_GenAIConvention(t *testing.T) {
	t0 := uint64(time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC).UnixNano())
	t1 := t0 + uint64(2*time.Second)
	sp := &tracepb.Span{
		TraceId: []byte{0x01, 0x02}, SpanId: []byte{0xaa}, Name: "chat",
		StartTimeUnixNano: t0, EndTimeUnixNano: t1,
		Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		Attributes: []*commonpb.KeyValue{
			kvStr("gen_ai.operation.name", "chat"),
			kvStr("gen_ai.system", "anthropic"),
			kvStr("gen_ai.response.model", "claude-opus-4-8"),
			kvInt("gen_ai.usage.input_tokens", 1000),
			kvInt("gen_ai.usage.output_tokens", 200),
			kvStr("gen_ai.response.id", "msg_abc123"),
			kvStr("gen_ai.conversation.id", "sess-9"),
		},
	}
	res := Map(export([]*commonpb.KeyValue{kvStr("sbo.tenant", "acme")}, sp), span.SourceOTLPTrace)

	if len(res.Spans) != 1 || len(res.Traces) != 1 {
		t.Fatalf("got %d spans %d traces, want 1/1", len(res.Spans), len(res.Traces))
	}
	s := res.Spans[0]
	if s.Kind != span.KindLLM {
		t.Errorf("kind = %q, want llm", s.Kind)
	}
	if s.Provider != "anthropic" || s.Model != "claude-opus-4-8" {
		t.Errorf("provider/model = %q/%q", s.Provider, s.Model)
	}
	if s.InputTokens == nil || *s.InputTokens != 1000 || s.OutputTokens == nil || *s.OutputTokens != 200 {
		t.Errorf("tokens = %v/%v, want 1000/200", s.InputTokens, s.OutputTokens)
	}
	if s.ProviderResponseID != "msg_abc123" {
		t.Errorf("response id = %q", s.ProviderResponseID)
	}
	// K1: no explicit request_id → falls back to response id (the key the
	// proxy commonly stored for Anthropic).
	if s.RequestID != "msg_abc123" {
		t.Errorf("request_id = %q, want msg_abc123 (response-id fallback)", s.RequestID)
	}
	if s.SpanID != "aa" || s.TraceID != "0102" {
		t.Errorf("ids = %q/%q, want aa/0102", s.SpanID, s.TraceID)
	}
	tr := res.Traces[0]
	if tr.SessionID != "sess-9" || tr.Tenant != "acme" || tr.RootSpanID != "aa" || tr.Status != span.StatusOK {
		t.Errorf("trace = %+v", tr)
	}
}

// TestMap_OpenInferenceConvention maps an Arize OpenInference span tree: an
// explicit span-kind, llm.* token/model keys, and a tool child.
func TestMap_OpenInferenceConvention(t *testing.T) {
	agent := &tracepb.Span{
		TraceId: []byte{0x07}, SpanId: []byte{0x01}, Name: "agent.run",
		Attributes: []*commonpb.KeyValue{kvStr("openinference.span.kind", "AGENT")},
	}
	llm := &tracepb.Span{
		TraceId: []byte{0x07}, SpanId: []byte{0x02}, ParentSpanId: []byte{0x01}, Name: "llm",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvStr("llm.provider", "openai"),
			kvStr("llm.model_name", "gpt-4o"),
			kvInt("llm.token_count.prompt", 50),
			kvInt("llm.token_count.completion", 25),
			kvStr("request_id", "req-xyz"),
		},
	}
	tool := &tracepb.Span{
		TraceId: []byte{0x07}, SpanId: []byte{0x03}, ParentSpanId: []byte{0x01}, Name: "search",
		Attributes: []*commonpb.KeyValue{kvStr("openinference.span.kind", "TOOL"), kvStr("tool.name", "search")},
	}
	res := Map(export(nil, agent, llm, tool), span.SourceSDKOTLP)

	if len(res.Spans) != 3 || len(res.Traces) != 1 {
		t.Fatalf("got %d spans %d traces, want 3/1", len(res.Spans), len(res.Traces))
	}
	m := bySpanID(res.Spans)
	if m["01"].Kind != span.KindAgent {
		t.Errorf("root kind = %q, want agent", m["01"].Kind)
	}
	if m["02"].Kind != span.KindLLM || m["02"].Provider != "openai" || m["02"].Model != "gpt-4o" {
		t.Errorf("llm span = %+v", m["02"])
	}
	if m["02"].RequestID != "req-xyz" {
		t.Errorf("explicit request_id = %q, want req-xyz", m["02"].RequestID)
	}
	if m["02"].InputTokens == nil || *m["02"].InputTokens != 50 {
		t.Errorf("prompt tokens = %v, want 50", m["02"].InputTokens)
	}
	if m["03"].Kind != span.KindTool {
		t.Errorf("tool kind = %q, want tool", m["03"].Kind)
	}
	// Tree: root is the parentless agent span.
	if res.Traces[0].RootSpanID != "01" || res.Traces[0].Source != span.SourceSDKOTLP {
		t.Errorf("trace root/source = %q/%q", res.Traces[0].RootSpanID, res.Traces[0].Source)
	}
}

// TestMap_OutputValueResponseID recovers the provider response id from the
// OpenInference `output.value` blob when no discrete response-id attribute is
// present — the live R4/R6/K1 finding (2026-06-28): raw
// openinference-instrumentation-openai buries the gen-… id inside the
// serialized response, so without this the span stays unlinked from its proxy
// turn. Both ProviderResponseID and the request_id soft-join key must adopt it.
func TestMap_OutputValueResponseID(t *testing.T) {
	outVal := `{"id":"gen-abc123","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	llm := &tracepb.Span{
		TraceId: []byte{0x11}, SpanId: []byte{0x02}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvStr("llm.model_name", "openai/gpt-4o-mini"),
			kvStr("output.value", outVal),
		},
	}
	res := Map(export(nil, llm), span.SourceOTLPTrace)
	if len(res.Spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(res.Spans))
	}
	s := res.Spans[0]
	if s.ProviderResponseID != "gen-abc123" {
		t.Errorf("ProviderResponseID = %q, want gen-abc123 (from output.value)", s.ProviderResponseID)
	}
	if s.RequestID != "gen-abc123" {
		t.Errorf("RequestID = %q, want gen-abc123 (response-id soft-join fallback)", s.RequestID)
	}

	// Precedence: an explicit response-id attribute wins over output.value.
	llm2 := &tracepb.Span{
		TraceId: []byte{0x11}, SpanId: []byte{0x03}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvStr("gen_ai.response.id", "explicit-id"),
			kvStr("output.value", outVal),
		},
	}
	// Malformed / id-less output.value yields no id (total, no panic).
	llm3 := &tracepb.Span{
		TraceId: []byte{0x11}, SpanId: []byte{0x04}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvStr("output.value", "not-json{"),
		},
	}
	m := bySpanID(Map(export(nil, llm2, llm3), span.SourceOTLPTrace).Spans)
	if m["03"].ProviderResponseID != "explicit-id" {
		t.Errorf("explicit response-id must win: got %q", m["03"].ProviderResponseID)
	}
	if m["04"].ProviderResponseID != "" || m["04"].RequestID != "" {
		t.Errorf("malformed output.value must yield no id: resp=%q req=%q", m["04"].ProviderResponseID, m["04"].RequestID)
	}
}

// TestMap_TokenDetail captures cache-read/cache-write/reasoning tokens from
// both the GenAI and OpenInference vocabularies (model-dependent attributes;
// absence yields a nil pointer, never a fabricated 0).
func TestMap_TokenDetail(t *testing.T) {
	// GenAI / Anthropic-style: discrete cache create+read.
	anth := &tracepb.Span{
		TraceId: []byte{0x21}, SpanId: []byte{0x01}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("gen_ai.system", "anthropic"),
			kvInt("gen_ai.usage.cache_read_input_tokens", 900),
			kvInt("gen_ai.usage.cache_creation_input_tokens", 120),
		},
	}
	// OpenInference / reasoning-model style: cache_read via prompt_details,
	// reasoning via completion_details.
	oi := &tracepb.Span{
		TraceId: []byte{0x21}, SpanId: []byte{0x02}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvInt("llm.token_count.prompt_details.cache_read", 50),
			kvInt("llm.token_count.completion_details.reasoning", 333),
		},
	}
	m := bySpanID(Map(export(nil, anth, oi), span.SourceOTLPTrace).Spans)
	a := m["01"]
	if a.CacheReadTokens == nil || *a.CacheReadTokens != 900 {
		t.Errorf("anth cache_read = %v, want 900", a.CacheReadTokens)
	}
	if a.CacheWriteTokens == nil || *a.CacheWriteTokens != 120 {
		t.Errorf("anth cache_write = %v, want 120", a.CacheWriteTokens)
	}
	if a.ReasoningTokens != nil {
		t.Errorf("anth reasoning must be nil (not emitted), got %v", *a.ReasoningTokens)
	}
	o := m["02"]
	if o.CacheReadTokens == nil || *o.CacheReadTokens != 50 {
		t.Errorf("oi cache_read = %v, want 50", o.CacheReadTokens)
	}
	if o.ReasoningTokens == nil || *o.ReasoningTokens != 333 {
		t.Errorf("oi reasoning = %v, want 333", o.ReasoningTokens)
	}
	if o.CacheWriteTokens != nil {
		t.Errorf("oi cache_write must be nil, got %v", *o.CacheWriteTokens)
	}
}

// TestMap_Content extracts prompt/response (LLM spans, both vocabularies) and
// tool I/O (tool spans), always with a content hash + raw body (the gate that
// strips raw is applied downstream in the ingestor, not here).
func TestMap_Content(t *testing.T) {
	// OpenInference LLM: indexed input/output messages.
	oiLLM := &tracepb.Span{
		TraceId: []byte{0x31}, SpanId: []byte{0x01}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvStr("llm.input_messages.0.message.role", "user"),
			kvStr("llm.input_messages.0.message.content", "what is 2+2?"),
			kvStr("llm.output_messages.0.message.role", "assistant"),
			kvStr("llm.output_messages.0.message.content", "4"),
		},
	}
	// GenAI LLM: serialized prompt/completion blobs (no indexed messages).
	genLLM := &tracepb.Span{
		TraceId: []byte{0x31}, SpanId: []byte{0x02}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("gen_ai.operation.name", "chat"),
			kvStr("input.value", `{"messages":[{"role":"user","content":"hi"}]}`),
			kvStr("output.value", `{"id":"gen-1","choices":[]}`),
		},
	}
	// Tool span: args + result fold into one tool_io JSON body.
	tool := &tracepb.Span{
		TraceId: []byte{0x31}, SpanId: []byte{0x03}, Name: "search",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "TOOL"),
			kvStr("input.value", `{"q":"weather"}`),
			kvStr("output.value", `{"temp":21}`),
		},
	}
	res := Map(export(nil, oiLLM, genLLM, tool), span.SourceOTLPTrace)

	byKind := map[string]map[span.ContentKind]span.SpanContent{}
	for _, c := range res.Content {
		if byKind[c.SpanID] == nil {
			byKind[c.SpanID] = map[span.ContentKind]span.SpanContent{}
		}
		byKind[c.SpanID][c.Kind] = c
	}

	// OpenInference LLM → prompt + response from the indexed message lists.
	p := byKind["01"][span.ContentPrompt]
	if p.Raw != `[{"role":"user","content":"what is 2+2?"}]` {
		t.Errorf("oi prompt raw = %q", p.Raw)
	}
	if p.ContentHash == "" {
		t.Errorf("oi prompt hash must always be set")
	}
	if r := byKind["01"][span.ContentResponse].Raw; r != `[{"role":"assistant","content":"4"}]` {
		t.Errorf("oi response raw = %q", r)
	}
	// GenAI LLM → prompt/response from the serialized value blobs.
	if g := byKind["02"][span.ContentPrompt].Raw; g != `{"messages":[{"role":"user","content":"hi"}]}` {
		t.Errorf("gen prompt raw = %q", g)
	}
	if g := byKind["02"][span.ContentResponse].Raw; g != `{"id":"gen-1","choices":[]}` {
		t.Errorf("gen response raw = %q", g)
	}
	// Tool span → a single tool_io body, no prompt/response.
	tio := byKind["03"][span.ContentToolIO]
	if tio.Raw != `{"input":"{\"q\":\"weather\"}","output":"{\"temp\":21}"}` {
		t.Errorf("tool_io raw = %q", tio.Raw)
	}
	if _, ok := byKind["03"][span.ContentPrompt]; ok {
		t.Errorf("tool span must not emit a prompt body")
	}
}

func kvDbl(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

// TestMap_ReportedCost captures the OpenInference llm.cost.* breakdown verbatim
// (no computation): components onto CostDetail, the reported total onto CostUSD,
// provenance = reported. A second span with components but NO total derives the
// total as the (non-overlapping) sum.
func TestMap_ReportedCost(t *testing.T) {
	withTotal := &tracepb.Span{
		TraceId: []byte{0x41}, SpanId: []byte{0x01}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvDbl("llm.cost.prompt_details.input", 0.002),
			kvDbl("llm.cost.completion_details.output", 0.004),
			kvDbl("llm.cost.prompt_details.cache_read", 0.0005),
			kvDbl("llm.cost.completion_details.reasoning", 0.001),
			kvDbl("llm.cost.total", 0.0075),
		},
	}
	sumOnly := &tracepb.Span{
		TraceId: []byte{0x41}, SpanId: []byte{0x02}, Name: "chat",
		Attributes: []*commonpb.KeyValue{
			kvStr("openinference.span.kind", "LLM"),
			kvDbl("llm.cost.prompt_details.input", 0.01),
			kvDbl("llm.cost.completion_details.output", 0.02),
		},
	}
	m := bySpanID(Map(export(nil, withTotal, sumOnly), span.SourceOTLPTrace).Spans)

	a := m["01"]
	if a.CostSource != span.CostReported {
		t.Errorf("cost_source = %q, want reported", a.CostSource)
	}
	if a.CostUSD == nil || *a.CostUSD != 0.0075 {
		t.Errorf("total = %v, want 0.0075 (reported)", a.CostUSD)
	}
	if a.CostDetail == nil || a.CostDetail.Input == nil || *a.CostDetail.Input != 0.002 ||
		a.CostDetail.Reasoning == nil || *a.CostDetail.Reasoning != 0.001 {
		t.Errorf("detail = %+v, want input 0.002 / reasoning 0.001", a.CostDetail)
	}

	b := m["02"]
	if b.CostUSD == nil || *b.CostUSD != 0.03 {
		t.Errorf("derived total = %v, want 0.03 (sum of components)", b.CostUSD)
	}
	if b.CostSource != span.CostReported {
		t.Errorf("sum-only cost_source = %q, want reported", b.CostSource)
	}
}

// TestMap_NoReportedCost leaves the cost fields zero so the ingestor's pricer
// fallback can fill them.
func TestMap_NoReportedCost(t *testing.T) {
	sp := &tracepb.Span{
		TraceId: []byte{0x42}, SpanId: []byte{0x01}, Name: "chat",
		Attributes: []*commonpb.KeyValue{kvStr("llm.model_name", "gpt-4o"), kvInt("llm.token_count.prompt", 100)},
	}
	s := bySpanID(Map(export(nil, sp), span.SourceOTLPTrace).Spans)["01"]
	if s.CostUSD != nil || s.CostSource != "" || s.CostDetail != nil {
		t.Errorf("unreported cost should be zero-valued, got cost=%v source=%q detail=%+v", s.CostUSD, s.CostSource, s.CostDetail)
	}
}

// TestMap_EchoGuard drops a ResourceSpans Observer emitted itself.
func TestMap_EchoGuard(t *testing.T) {
	sp := &tracepb.Span{TraceId: []byte{0x09}, SpanId: []byte{0x01}, Name: "x"}
	req := export([]*commonpb.KeyValue{kvStr("sbo.emitted_by", "observer")}, sp)
	res := Map(req, span.SourceOTLPTrace)
	if len(res.Spans) != 0 || len(res.Traces) != 0 {
		t.Errorf("echo-guarded export produced %d spans %d traces, want 0/0", len(res.Spans), len(res.Traces))
	}
}

// TestMap_EventsAndLinks captures span events + cross-trace links.
func TestMap_EventsAndLinks(t *testing.T) {
	sp := &tracepb.Span{
		TraceId: []byte{0x0a}, SpanId: []byte{0x01}, Name: "x",
		Events: []*tracepb.Span_Event{{Name: "exception", TimeUnixNano: 42, Attributes: []*commonpb.KeyValue{kvStr("exception.type", "ValueError")}}},
		Links:  []*tracepb.Span_Link{{TraceId: []byte{0x0b}, SpanId: []byte{0x02}}},
	}
	res := Map(export(nil, sp), span.SourceOTLPTrace)
	if len(res.Events) != 1 || res.Events[0].Name != "exception" || res.Events[0].SpanID != "01" {
		t.Fatalf("events = %+v", res.Events)
	}
	if res.Events[0].AttributesJSON != `{"exception.type":"ValueError"}` {
		t.Errorf("event attrs json = %q", res.Events[0].AttributesJSON)
	}
	if len(res.Links) != 1 || res.Links[0].LinkedTrace != "0b" || res.Links[0].LinkedSpan != "02" {
		t.Errorf("links = %+v", res.Links)
	}
}

// TestMap_Empty is total on nil / empty input.
func TestMap_Empty(t *testing.T) {
	if r := Map(nil, span.SourceOTLPTrace); len(r.Spans) != 0 || len(r.Traces) != 0 {
		t.Errorf("nil req should map empty, got %+v", r)
	}
	if r := Map(&coltracepb.ExportTraceServiceRequest{}, span.SourceOTLPTrace); len(r.Spans) != 0 {
		t.Errorf("empty req should map empty")
	}
}
