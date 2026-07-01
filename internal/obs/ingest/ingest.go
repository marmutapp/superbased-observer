// Package ingest is the PURE boundary mapper for the observability
// subsystem: it decodes an already-unmarshaled OTLP trace export into the
// canonical obs span/trace model (internal/obs/span). No database/sql,
// net/http, or fsnotify (pinned by imports_test.go) — persistence and the
// network receiver live elsewhere.
//
// It normalizes BOTH conventions the ecosystem emits — OpenTelemetry GenAI
// (`gen_ai.*`) and Arize OpenInference (`llm.*` / `openinference.span.kind`)
// — into ONE canonical shape at this boundary (CLAUDE.md module rule #3), via
// multi-candidate-key attribute lookups (instrumentor key names drift across
// versions, the ccotel pattern). Downstream code never branches on the source
// convention again.
//
// Echo-guard parity (plan §5.4): a ResourceSpans whose resource carries
// `sbo.emitted_by=observer` is skipped, so Observer never re-ingests telemetry
// it emitted itself.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	emittedByAttr = "sbo.emitted_by"
	emittedBySelf = "observer"
)

// Result is the canonical output of mapping one OTLP trace export.
type Result struct {
	Traces  []span.Trace
	Spans   []span.Span
	Events  []span.SpanEvent
	Links   []span.SpanLink
	Content []span.SpanContent
}

// candidate-key lists. Order = precedence (first present wins). GenAI and
// OpenInference names are interleaved so either convention resolves.
var (
	keysModel     = []string{"gen_ai.response.model", "gen_ai.request.model", "llm.model_name", "llm.model", "model"}
	keysProvider  = []string{"gen_ai.system", "llm.provider", "llm.system", "provider"}
	keysInputTok  = []string{"gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.token_count.prompt", "llm.usage.prompt_tokens"}
	keysOutputTok = []string{"gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.token_count.completion", "llm.usage.completion_tokens"}
	keysTotalTok  = []string{"gen_ai.usage.total_tokens", "llm.token_count.total"}
	// Token detail (model-dependent — read whatever discrete attribute the
	// instrumentor emits; absence is an honest NULL, never fabricated).
	keysCacheRead  = []string{"gen_ai.usage.cache_read_input_tokens", "gen_ai.usage.cached_tokens", "llm.token_count.prompt_details.cache_read", "llm.token_count.cache_read"}
	keysCacheWrite = []string{"gen_ai.usage.cache_creation_input_tokens", "llm.token_count.prompt_details.cache_write", "llm.token_count.cache_write"}
	keysReasoning  = []string{"gen_ai.usage.reasoning_tokens", "gen_ai.usage.completion_tokens_details.reasoning_tokens", "llm.token_count.completion_details.reasoning", "llm.token_count.reasoning"}
	keysCost       = []string{"gen_ai.usage.cost", "llm.cost.total", "cost.total"}
	// Reported per-component cost (USD). OpenInference's llm.cost.* namespace
	// emits a full breakdown; GenAI carries only a total. Captured verbatim
	// (no computation) — the SpanPricer fallback runs ONLY when none of these
	// are present. Prefer the prompt_details.input / completion_details.output
	// components over the top-level prompt / completion totals so the captured
	// components never overlap the cache columns.
	keysCostInput     = []string{"llm.cost.prompt_details.input", "llm.cost.prompt", "gen_ai.usage.input_cost"}
	keysCostOutput    = []string{"llm.cost.completion_details.output", "llm.cost.completion", "gen_ai.usage.output_cost"}
	keysCostCacheRead = []string{"llm.cost.prompt_details.cache_read"}
	keysCostCacheWr   = []string{"llm.cost.prompt_details.cache_write"}
	keysCostReasoning = []string{"llm.cost.completion_details.reasoning"}
	keysCostTool      = []string{"llm.cost.tool", "gen_ai.usage.tool_cost"}
	keysRequestID     = []string{"gen_ai.request.id", "request.id", "request_id", "http.request.header.x-request-id"}
	keysResponseID    = []string{"gen_ai.response.id", "llm.response.id", "response.id"}
	keysOutputValue   = []string{"output.value", "gen_ai.output.value"}
	keysToolCallID    = []string{"gen_ai.tool.call.id", "tool_call.id", "tool.call_id"}
	keysSession       = []string{"session.id", "gen_ai.conversation.id", "conversation.id", "thread.id"}
	keysThread        = []string{"thread.id", "gen_ai.thread.id"}
	keysTenant        = []string{"sbo.tenant", "tenant.id", "tenant"}
	keysUser          = []string{"sbo.user", "enduser.id", "user.id", "user"}
	keysProjectRoot   = []string{"sbo.project_root", "project.root"}
	keysToolName      = []string{"tool.name", "gen_ai.tool.name"}
	keysOperation     = []string{"gen_ai.operation.name"}
	// Content bodies (model-/instrumentor-dependent). For an LLM span we
	// prefer the OpenInference indexed message lists, then GenAI's, then the
	// serialized input/output value blobs. For a tool span the args/result
	// live in input.value/tool.parameters + output.value. Absence yields no
	// content row (an honest gap, never a fabricated body).
	keysPromptValue   = []string{"input.value", "gen_ai.prompt", "llm.prompt", "prompt"}
	keysResponseValue = []string{"output.value", "gen_ai.output.value", "gen_ai.completion", "llm.completion", "completion"}
	keysToolArgs      = []string{"input.value", "tool.parameters", "gen_ai.tool.input", "tool_call.function.arguments"}
	keysToolResult    = []string{"output.value", "gen_ai.tool.output", "tool.result", "tool_call.result"}
)

// OpenInference + GenAI indexed-message attribute shapes. A flattened message
// list is "<prefix>.<i><roleSuffix>" / "<prefix>.<i><contentSuffix>".
const (
	oiInputPrefix    = "llm.input_messages"
	oiOutputPrefix   = "llm.output_messages"
	oiRoleSuffix     = ".message.role"
	oiContentSuffix  = ".message.content"
	genInputPrefix   = "gen_ai.prompt"
	genOutputPrefix  = "gen_ai.completion"
	genRoleSuffix    = ".role"
	genContentSuffix = ".content"
	maxIndexedMsgs   = 512
)

// Map normalizes one OTLP trace export into the canonical model, tagging every
// trace/span with src. It is total: a malformed/empty export yields an empty
// Result, never a panic.
func Map(req *coltracepb.ExportTraceServiceRequest, src span.Source) Result {
	var res Result
	if req == nil {
		return res
	}
	// Per-trace aggregation for synthesizing obs_traces rows.
	type agg struct {
		minStart, maxEnd time.Time
		root             string
		rootStart        time.Time
		session, thread  string
		tenant, user     string
		projectRoot      string
		anyError         bool
	}
	traces := map[string]*agg{}
	order := []string{}

	for _, rs := range req.GetResourceSpans() {
		resAttrs := attrMap(rs.GetResource().GetAttributes())
		if str(resAttrs, emittedByAttr) == emittedBySelf {
			continue // echo guard
		}
		rTenant := firstStr(resAttrs, keysTenant)
		rUser := firstStr(resAttrs, keysUser)
		rSession := firstStr(resAttrs, keysSession)
		rProject := firstStr(resAttrs, keysProjectRoot)

		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				traceID := hexID(sp.GetTraceId())
				spanID := hexID(sp.GetSpanId())
				if traceID == "" || spanID == "" {
					continue
				}
				a := attrMap(sp.GetAttributes())
				start := unixNano(sp.GetStartTimeUnixNano())
				end := unixNano(sp.GetEndTimeUnixNano())
				status := mapStatus(sp.GetStatus())

				cs := span.Span{
					SpanID:             spanID,
					TraceID:            traceID,
					ParentSpanID:       hexID(sp.GetParentSpanId()),
					Kind:               mapKind(a),
					Name:               sp.GetName(),
					Status:             status,
					StartedAt:          start,
					EndedAt:            end,
					Model:              firstStr(a, keysModel),
					Provider:           firstStr(a, keysProvider),
					InputTokens:        firstInt(a, keysInputTok),
					OutputTokens:       firstInt(a, keysOutputTok),
					TotalTokens:        firstInt(a, keysTotalTok),
					CacheReadTokens:    firstInt(a, keysCacheRead),
					CacheWriteTokens:   firstInt(a, keysCacheWrite),
					ReasoningTokens:    firstInt(a, keysReasoning),
					RequestID:          firstStr(a, keysRequestID),
					ProviderResponseID: firstStr(a, keysResponseID),
					ToolCallID:         firstStr(a, keysToolCallID),
					Source:             src,
				}
				// Raw OpenInference instrumentors (e.g.
				// openinference-instrumentation-openai) emit no discrete
				// response-id attribute — the provider id lives ONLY inside
				// the serialized response under output.value. When no explicit
				// response-id attribute is present, recover it from there so
				// the span can still soft-join its proxy turn (live-validated
				// 2026-06-28: the gen-… id was recoverable on every LLM span).
				if cs.ProviderResponseID == "" {
					cs.ProviderResponseID = responseIDFromOutputValue(a)
				}
				// K1 mitigation: when no explicit request_id attribute is
				// present, fall back to the provider response id as the
				// soft join key — the proxy commonly stored exactly that
				// (Anthropic msg_… / OpenAI-style gen-…). Live validation
				// gates "verified".
				if cs.RequestID == "" {
					cs.RequestID = cs.ProviderResponseID
				}
				if cs.Name == "" {
					cs.Name = firstStr(a, keysToolName)
				}
				// Reported cost detail (no computation — the SpanPricer
				// fallback in the ingestor fires only when this is absent).
				applyReportedCost(a, &cs)
				res.Spans = append(res.Spans, cs)
				res.Content = append(res.Content, extractContent(a, cs)...)

				for _, ev := range sp.GetEvents() {
					res.Events = append(res.Events, span.SpanEvent{
						SpanID:         spanID,
						Time:           unixNano(ev.GetTimeUnixNano()),
						Name:           ev.GetName(),
						AttributesJSON: attrsJSON(ev.GetAttributes()),
					})
				}
				for _, lk := range sp.GetLinks() {
					res.Links = append(res.Links, span.SpanLink{
						SpanID:         spanID,
						LinkedTrace:    hexID(lk.GetTraceId()),
						LinkedSpan:     hexID(lk.GetSpanId()),
						AttributesJSON: attrsJSON(lk.GetAttributes()),
					})
				}

				// Aggregate into the trace summary.
				t, ok := traces[traceID]
				if !ok {
					t = &agg{}
					traces[traceID] = t
					order = append(order, traceID)
				}
				if !start.IsZero() && (t.minStart.IsZero() || start.Before(t.minStart)) {
					t.minStart = start
				}
				if end.After(t.maxEnd) {
					t.maxEnd = end
				}
				if cs.ParentSpanID == "" && (t.root == "" || (!start.IsZero() && start.Before(t.rootStart))) {
					t.root = spanID
					t.rootStart = start
				}
				if status == span.StatusError {
					t.anyError = true
				}
				t.session = coalesce(t.session, firstStr(a, keysSession), rSession)
				t.thread = coalesce(t.thread, firstStr(a, keysThread))
				t.tenant = coalesce(t.tenant, firstStr(a, keysTenant), rTenant)
				t.user = coalesce(t.user, firstStr(a, keysUser), rUser)
				t.projectRoot = coalesce(t.projectRoot, firstStr(a, keysProjectRoot), rProject)
			}
		}
	}

	for _, id := range order {
		t := traces[id]
		st := span.StatusUnset
		if t.anyError {
			st = span.StatusError
		} else if !t.maxEnd.IsZero() {
			st = span.StatusOK
		}
		res.Traces = append(res.Traces, span.Trace{
			TraceID:     id,
			SessionID:   t.session,
			ThreadID:    t.thread,
			Tenant:      t.tenant,
			User:        t.user,
			Source:      src,
			RootSpanID:  t.root,
			ProjectRoot: t.projectRoot,
			Status:      st,
			StartedAt:   t.minStart,
			EndedAt:     t.maxEnd,
		})
	}
	return res
}

// mapKind resolves the canonical span kind from attributes: OpenInference's
// explicit span-kind wins; else the GenAI operation name; else a model
// attribute implies an llm span; else a generic chain/event.
func mapKind(a map[string]*commonpb.AnyValue) span.Kind {
	switch normKind(str(a, "openinference.span.kind")) {
	case "LLM":
		return span.KindLLM
	case "TOOL":
		return span.KindTool
	case "RETRIEVER", "RERANKER":
		return span.KindRetriever
	case "EMBEDDING":
		return span.KindEmbedding
	case "CHAIN":
		return span.KindChain
	case "AGENT":
		return span.KindAgent
	case "GUARDRAIL":
		return span.KindGuardrail
	case "EVALUATOR":
		return span.KindEvaluator
	}
	switch firstStr(a, keysOperation) {
	case "chat", "text_completion", "generate_content":
		return span.KindLLM
	case "embeddings":
		return span.KindEmbedding
	}
	if firstStr(a, keysModel) != "" {
		return span.KindLLM
	}
	if firstStr(a, keysToolName) != "" {
		return span.KindTool
	}
	return span.KindChain
}

// ---- attribute helpers ----

func attrMap(kvs []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	m := make(map[string]*commonpb.AnyValue, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		m[kv.GetKey()] = kv.GetValue()
	}
	return m
}

func str(m map[string]*commonpb.AnyValue, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	}
	return ""
}

func firstStr(m map[string]*commonpb.AnyValue, keys []string) string {
	for _, k := range keys {
		if s := str(m, k); s != "" {
			return s
		}
	}
	return ""
}

// responseIDFromOutputValue recovers a provider response id from the
// OpenInference `output.value` attribute, which carries the full
// serialized chat-completion response as a JSON string. Raw instrumentors
// emit no discrete response-id attribute, so this is the only place the id
// (OpenAI `chatcmpl-…` / OpenRouter `gen-…`) survives. Total: a missing,
// non-JSON, or id-less value yields "" and never panics.
func responseIDFromOutputValue(m map[string]*commonpb.AnyValue) string {
	raw := firstStr(m, keysOutputValue)
	if raw == "" {
		return ""
	}
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return ""
	}
	return env.ID
}

// extractContent pulls the prompt/response (LLM spans) or tool args/result
// (tool spans) bodies out of a span's attributes into content rows. It always
// computes a content hash and carries the raw body; the gate that decides
// whether the raw survives persistence is applied downstream in the ingestor
// (the mapper stays pure — no ContentGate dependency). Branches on span KIND
// (a capability classification), never on tool/source identity (rule #3).
// Total: a content-less span yields no rows and never panics.
func extractContent(a map[string]*commonpb.AnyValue, sp span.Span) []span.SpanContent {
	var out []span.SpanContent
	add := func(kind span.ContentKind, raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		out = append(out, span.SpanContent{
			SpanID:      sp.SpanID,
			TraceID:     sp.TraceID,
			RequestID:   sp.RequestID,
			Kind:        kind,
			ContentHash: hashContent(raw),
			Raw:         raw,
			Time:        sp.StartedAt,
		})
	}
	switch sp.Kind {
	case span.KindTool:
		in := firstStr(a, keysToolArgs)
		res := firstStr(a, keysToolResult)
		add(span.ContentToolIO, toolIOJSON(in, res))
	case span.KindLLM:
		add(span.ContentPrompt, promptBody(a))
		add(span.ContentResponse, responseBody(a))
	}
	return out
}

// promptBody resolves the request-side content: OpenInference indexed input
// messages first, then GenAI's, then the serialized input.value blob.
func promptBody(a map[string]*commonpb.AnyValue) string {
	if s := collectIndexedMessages(a, oiInputPrefix, oiRoleSuffix, oiContentSuffix); s != "" {
		return s
	}
	if s := collectIndexedMessages(a, genInputPrefix, genRoleSuffix, genContentSuffix); s != "" {
		return s
	}
	return firstStr(a, keysPromptValue)
}

// responseBody resolves the response-side content, mirroring promptBody.
func responseBody(a map[string]*commonpb.AnyValue) string {
	if s := collectIndexedMessages(a, oiOutputPrefix, oiRoleSuffix, oiContentSuffix); s != "" {
		return s
	}
	if s := collectIndexedMessages(a, genOutputPrefix, genRoleSuffix, genContentSuffix); s != "" {
		return s
	}
	return firstStr(a, keysResponseValue)
}

// collectIndexedMessages folds a flattened "<prefix>.<i>..." message list into
// a compact JSON array of {role,content}. Returns "" when no indexed message
// is present (so the caller can fall back to a serialized value blob).
func collectIndexedMessages(a map[string]*commonpb.AnyValue, prefix, roleSuffix, contentSuffix string) string {
	type msg struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	}
	var msgs []msg
	for i := 0; i < maxIndexedMsgs; i++ {
		idx := prefix + "." + strconv.Itoa(i)
		role := str(a, idx+roleSuffix)
		content := str(a, idx+contentSuffix)
		if role == "" && content == "" {
			break
		}
		msgs = append(msgs, msg{Role: role, Content: content})
	}
	if len(msgs) == 0 {
		return ""
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	return string(b)
}

// toolIOJSON renders a tool span's args + result as a single {input,output}
// JSON object (only the present halves). Returns "" when both are empty.
func toolIOJSON(in, out string) string {
	m := make(map[string]string, 2)
	if in != "" {
		m["input"] = in
	}
	if out != "" {
		m["output"] = out
	}
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// applyReportedCost reads any instrumentor-reported cost (the OpenInference
// llm.cost.* breakdown or a GenAI total) onto the span: it sets CostDetail to
// the present components, CostUSD to the reported total (or, when no total is
// given, the sum of the captured components — which never overlap because the
// input component prefers prompt_details.input over the cache columns), and
// marks CostSource = reported. When NOTHING is reported it leaves the cost
// fields zero so the ingestor's SpanPricer fallback can compute them. No
// computation happens here.
func applyReportedCost(a map[string]*commonpb.AnyValue, sp *span.Span) {
	bd := span.CostBreakdown{
		Input:      firstFloat(a, keysCostInput),
		Output:     firstFloat(a, keysCostOutput),
		CacheRead:  firstFloat(a, keysCostCacheRead),
		CacheWrite: firstFloat(a, keysCostCacheWr),
		Reasoning:  firstFloat(a, keysCostReasoning),
		Tool:       firstFloat(a, keysCostTool),
	}
	total := firstFloat(a, keysCost)
	if total == nil && !bd.Empty() {
		total = sumComponents(bd)
	}
	if total == nil && bd.Empty() {
		return // nothing reported; leave for the computed fallback
	}
	sp.CostUSD = total
	sp.CostSource = span.CostReported
	if !bd.Empty() {
		sp.CostDetail = &bd
	}
}

// sumComponents adds the present components of a breakdown into a total. The
// input component is the non-cached prompt cost (prompt_details.input, or the
// top-level prompt when details are absent), so summing it with the cache and
// output components does not double-count.
func sumComponents(bd span.CostBreakdown) *float64 {
	var sum float64
	for _, c := range []*float64{bd.Input, bd.Output, bd.CacheRead, bd.CacheWrite, bd.Reasoning, bd.Tool} {
		if c != nil {
			sum += *c
		}
	}
	return &sum
}

// hashContent is the content-free signal stored alongside (or instead of) a raw
// body — a SHA-256 hex digest of the body bytes.
func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func firstInt(m map[string]*commonpb.AnyValue, keys []string) *int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch x := v.GetValue().(type) {
		case *commonpb.AnyValue_IntValue:
			n := x.IntValue
			return &n
		case *commonpb.AnyValue_DoubleValue:
			n := int64(x.DoubleValue)
			return &n
		case *commonpb.AnyValue_StringValue:
			if n, err := strconv.ParseInt(x.StringValue, 10, 64); err == nil {
				return &n
			}
		}
	}
	return nil
}

func firstFloat(m map[string]*commonpb.AnyValue, keys []string) *float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch x := v.GetValue().(type) {
		case *commonpb.AnyValue_DoubleValue:
			f := x.DoubleValue
			return &f
		case *commonpb.AnyValue_IntValue:
			f := float64(x.IntValue)
			return &f
		case *commonpb.AnyValue_StringValue:
			if f, err := strconv.ParseFloat(x.StringValue, 64); err == nil {
				return &f
			}
		}
	}
	return nil
}

// attrsJSON renders span-event / link attributes as a compact metadata-only
// JSON object (scalar values; nested structures stringified). Best-effort:
// returns "" on any marshal error.
func attrsJSON(kvs []*commonpb.KeyValue) string {
	if len(kvs) == 0 {
		return ""
	}
	m := attrMap(kvs)
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = str(m, k)
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(out))
	for _, k := range keys {
		ordered[k] = out[k]
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		return ""
	}
	return string(b)
}

func mapStatus(s *tracepb.Status) span.Status {
	if s == nil {
		return span.StatusUnset
	}
	switch s.GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		return span.StatusOK
	case tracepb.Status_STATUS_CODE_ERROR:
		return span.StatusError
	default:
		return span.StatusUnset
	}
}

func hexID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

func unixNano(ns uint64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns)).UTC()
}

// normKind upper-cases an OpenInference span-kind value (instrumentors emit
// either case), trimming whitespace, so the mapKind switch is case-stable.
func normKind(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// coalesce returns the first non-empty string.
func coalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
