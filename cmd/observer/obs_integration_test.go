//go:build !no_obs

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	otlpingest "github.com/marmutapp/superbased-observer/internal/ingest/otlp"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/httpapi"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
	"github.com/marmutapp/superbased-observer/internal/store"

	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestObs_LiveHTTPIngestAndDedup is the real network integration the unit
// tests stop short of: it stands up the actual OTLP receiver with the REAL
// obs trace ingestor + the REAL host obsTurnSink, then POSTs protobuf OTLP
// traces over HTTP and asserts the full receiver → mapper → store → api_turns
// reconciliation, INCLUDING proxy-collision dedup by fidelity. It exercises
// every P2 seam end to end exactly as `observer start` wires them (minus the
// cobra/errgroup glue). The one thing it cannot prove — that a real
// third-party instrumentor emits the same request_id the proxy stored (K1) —
// is a fact about external software, documented as the open runtime gate.
func TestObs_LiveHTTPIngestAndDedup(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st := store.New(conn)
	obsStore, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	ingestor := obs.NewTraceIngestor(obsStore, obsTurnSink{st: st}, slog.Default())

	recv, err := otlpingest.New(otlpingest.Options{HTTPAddr: "127.0.0.1:0", TraceHandler: ingestor.Ingest})
	if err != nil {
		t.Fatalf("receiver New: %v", err)
	}
	recv.Start()
	defer func() { _ = recv.Shutdown(context.Background()) }()
	url := "http://" + recv.HTTPAddr() + "/v1/traces"

	post := func(req *coltracepb.ExportTraceServiceRequest) {
		t.Helper()
		raw, _ := proto.Marshal(req)
		resp, err := http.Post(url, "application/x-protobuf", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}

	// --- Case A: a fresh GenAI llm span with no prior proxy turn. ---
	post(traceReq(0x11, 0x22, map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.system":         "anthropic",
		"gen_ai.response.model": "claude-opus-4-8",
		"gen_ai.response.id":    "msg_live_A", // no explicit request_id → fallback
	}, map[string]int64{"gen_ai.usage.input_tokens": 300, "gen_ai.usage.output_tokens": 40}))

	if got := scanInt(t, conn, `SELECT COUNT(*) FROM obs_spans WHERE request_id='msg_live_A'`); got != 1 {
		t.Errorf("obs_spans for msg_live_A = %d, want 1", got)
	}
	// Reconciled into api_turns as an Approx obs turn (no proxy row existed).
	in, src := scanTurn(t, conn, "msg_live_A")
	if in != 300 || src != "otlp_trace" {
		t.Errorf("api_turn msg_live_A = (input %d, source %q), want (300, otlp_trace)", in, src)
	}

	// --- Case B: a proxy turn already exists; the obs span must DEDUP into
	// it and the proxy's exact tokens must WIN (fidelity), not double-count. ---
	if _, err := st.InsertAPITurn(ctx, models.APITurn{
		RequestID: "msg_live_B", Source: "proxy", Provider: "anthropic",
		Model: "claude-opus-4-8", InputTokens: 9999, OutputTokens: 1234,
		CostUSD: 0.50, Timestamp: time.Now().UTC(), HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("seed proxy turn: %v", err)
	}
	post(traceReq(0x33, 0x44, map[string]string{
		"openinference.span.kind": "LLM",
		"llm.provider":            "anthropic",
		"llm.model_name":          "claude-opus-4-8",
		"gen_ai.response.id":      "msg_live_B",
	}, map[string]int64{"llm.token_count.prompt": 100, "llm.token_count.completion": 5}))

	if got := scanInt(t, conn, `SELECT COUNT(*) FROM api_turns WHERE request_id='msg_live_B'`); got != 1 {
		t.Fatalf("api_turns for msg_live_B = %d, want 1 (obs span must dedup into the proxy turn)", got)
	}
	in2, src2 := scanTurn(t, conn, "msg_live_B")
	if in2 != 9999 || src2 != "proxy" {
		t.Errorf("deduped turn = (input %d, source %q), want (9999, proxy) — proxy fidelity must win", in2, src2)
	}
	// The obs span itself still persisted (topology is additive).
	if got := scanInt(t, conn, `SELECT COUNT(*) FROM obs_spans WHERE request_id='msg_live_B'`); got != 1 {
		t.Errorf("obs_spans for msg_live_B = %d, want 1", got)
	}
}

// TestObs_ProxyEnrichmentEndToEnd grounds P6 (§9) through the REAL host path:
// the real store seam (EnrichmentByRequestID joining api_turns + router_decisions),
// the real obsProxyEnricher, and the real /api/obs/trace/{id} handler. It proves
// an LLM span the proxy also saw renders proxy-exact cost + cache split +
// routing rationale, pull-only — the wedge no competitor can show on an
// arbitrary agent's trajectory. (The httpapi unit test covers the same shape
// with a fake enricher; this one exercises the host's actual SQL join.)
func TestObs_ProxyEnrichmentEndToEnd(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st := store.New(conn)
	obsStore, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}

	// A real proxy turn (exact tokens + cache split + cost) + a routing decision.
	_, turnID, err := st.UpsertTurnByRequestID(ctx, models.APITurn{
		RequestID: "chatcmpl-e2e", Source: "proxy", Provider: "openai", Model: "gpt-4o",
		InputTokens: 320, OutputTokens: 48, CacheReadTokens: 256, CacheCreationTokens: 64,
		CostUSD: 0.0210, Timestamp: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed proxy turn: %v", err)
	}
	if err := st.InsertRouterDecisions(ctx, []store.RouterDecisionRow{{
		APITurnID: &turnID, Timestamp: time.Now().UTC(), Mode: "enforce", Channel: "proxy",
		OriginalModel: "gpt-4o", SelectedModel: "gpt-4o-mini", TurnKind: "edit",
		PolicyHash: "h1", ReasonCodes: []string{"downshift"},
	}}); err != nil {
		t.Fatalf("seed routing: %v", err)
	}
	// A guard verdict anchored to the same api_turn (the proxy response-
	// inspection anchor) — P6 GuardVerdict follow-up.
	if _, err := st.InsertGuardEvents(ctx, []store.GuardEventRow{{
		TS: time.Now().UTC(), SessionID: "s1", APITurnID: &turnID,
		RuleID: "R-172", Category: "destructive", Severity: "high", Decision: "flag",
		Source: "proxy", Reason: "rm -rf ~",
	}}); err != nil {
		t.Fatalf("seed guard event: %v", err)
	}

	// An obs trace whose LLM span carries that request_id.
	start := time.Now().UTC()
	if err := obsStore.UpsertTrace(ctx, span.Trace{
		TraceID: "te2e", Source: span.SourceOTLPTrace, RootSpanID: "root",
		Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second),
	}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	in := int64(320)
	out := int64(48)
	if err := obsStore.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "root", TraceID: "te2e", Kind: span.KindAgent, Name: "run", StartedAt: start, EndedAt: start.Add(time.Second)},
		{
			SpanID: "llm", TraceID: "te2e", ParentSpanID: "root", Kind: span.KindLLM, Name: "chat",
			Model: "gpt-4o", InputTokens: &in, OutputTokens: &out, RequestID: "chatcmpl-e2e",
			StartedAt: start, EndedAt: start.Add(time.Second),
		},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}

	// The REAL handler with the REAL host enricher.
	api := httpapi.New(obsStore, obsProxyEnricher{st: st}, slog.Default())
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/obs/trace/te2e")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var d struct {
		Enrichments map[string]obs.Enrichment `json:"enrichments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := d.Enrichments["llm"]
	if !ok {
		t.Fatalf("no enrichment for llm span: %+v", d.Enrichments)
	}
	if !got.Found || got.CostUSD != 0.0210 || got.CacheReadTokens != 256 || got.CacheCreationTokens != 64 {
		t.Errorf("enrichment cost/cache = %+v", got)
	}
	if got.RoutingReason != "→ gpt-4o-mini (downshift)" {
		t.Errorf("RoutingReason = %q", got.RoutingReason)
	}
	if got.GuardVerdict != "flag R-172 (destructive)" {
		t.Errorf("GuardVerdict = %q, want %q (joined via api_turn_id anchor)", got.GuardVerdict, "flag R-172 (destructive)")
	}
	// The root (agent) span has no request_id → never enriched.
	if _, ok := d.Enrichments["root"]; ok {
		t.Errorf("root span unexpectedly enriched")
	}
}

func traceReq(traceByte, spanByte byte, strs map[string]string, ints map[string]int64) *coltracepb.ExportTraceServiceRequest {
	attrs := make([]*commonpb.KeyValue, 0, len(strs)+len(ints))
	for k, v := range strs {
		attrs = append(attrs, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}})
	}
	for k, v := range ints {
		attrs = append(attrs, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}})
	}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{TraceId: []byte{traceByte}, SpanId: []byte{spanByte}, Name: "chat", Attributes: attrs}},
			}},
		}},
	}
}

func scanInt(t *testing.T, conn *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("scanInt %q: %v", q, err)
	}
	return n
}

func scanTurn(t *testing.T, conn *sql.DB, reqID string) (int64, string) {
	t.Helper()
	var in int64
	var src string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT input_tokens, COALESCE(source,'') FROM api_turns WHERE request_id=?`, reqID).Scan(&in, &src); err != nil {
		t.Fatalf("scanTurn %q: %v", reqID, err)
	}
	return in, src
}
