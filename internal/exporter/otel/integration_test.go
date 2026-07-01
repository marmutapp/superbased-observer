package otel

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// fakeCollector is a minimal OTLP/HTTP trace receiver: it decodes the protobuf
// ExportTraceServiceRequest the exporter POSTs to /v1/traces and records the
// received spans + resource attributes for field-by-field assertion.
type fakeCollector struct {
	mu       sync.Mutex
	spans    []*tracepb.Span
	resAttrs []*commonpb.KeyValue
}

func (c *fakeCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c.mu.Lock()
	for _, rs := range req.GetResourceSpans() {
		if rs.GetResource() != nil {
			c.resAttrs = append(c.resAttrs, rs.GetResource().GetAttributes()...)
		}
		for _, ss := range rs.GetScopeSpans() {
			c.spans = append(c.spans, ss.GetSpans()...)
		}
	}
	c.mu.Unlock()

	out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(out)
}

func (c *fakeCollector) snapshot() ([]*tracepb.Span, []*commonpb.KeyValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...), append([]*commonpb.KeyValue(nil), c.resAttrs...)
}

func pbAttrs(kvs []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	m := make(map[string]*commonpb.AnyValue, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = kv.GetValue()
	}
	return m
}

// TestExporter_OTLPHTTPWireShape drives the real exporter against a fake OTLP/
// HTTP collector and asserts the received spans match the expected attribute
// set on the wire — the protobuf round-trip the in-memory unit tests cannot
// cover.
func TestExporter_OTLPHTTPWireShape(t *testing.T) {
	c := &fakeCollector{}
	srv := httptest.NewServer(c)
	defer srv.Close()

	now := time.Now()
	src := &fakeSource{turns: []models.APITurn{
		{
			ID: 1, Provider: "anthropic", Model: "claude-opus-4-7", RequestID: "req_1",
			InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20, CostUSD: 0.05,
			ProjectID: 3, SessionID: "sess-9", StopReason: "end_turn",
			TotalResponseMS: 800, OrgID: "org-x", UserEmail: "dev@acme.com",
			Timestamp: now,
		},
		{
			ID: 2, Provider: "openai", Model: "gpt-x", HTTPStatus: 500,
			ErrorClass: "server_error", ErrorMessage: "boom", ProjectID: 3, Timestamp: now,
		},
	}}
	res := &fakeResolver{roots: map[int64]string{3: "/srv/app"}}

	// emit_user_email ON to verify the gated attribute crosses the wire.
	e, err := New(Config{Endpoint: srv.URL, Insecure: true, EmitUserEmail: true}, src, res, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Shutdown force-flushes the batch processor and waits for the POST.
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	spans, resAttrs := c.snapshot()
	if len(spans) != 2 {
		t.Fatalf("collector received %d spans, want 2", len(spans))
	}
	if pbAttrs(resAttrs)["service.name"].GetStringValue() != defaultServiceName {
		t.Errorf("resource service.name = %q, want %q",
			pbAttrs(resAttrs)["service.name"].GetStringValue(), defaultServiceName)
	}

	byName := map[string]*tracepb.Span{}
	for _, s := range spans {
		byName[s.GetName()] = s
	}

	ok := byName["chat claude-opus-4-7"]
	if ok == nil {
		t.Fatalf("missing span 'chat claude-opus-4-7'; got %v", names(spans))
	}
	a := pbAttrs(ok.GetAttributes())
	wantStr := map[string]string{
		keyGenAIProviderName:  "anthropic",
		keyGenAIOperationName: operationChat,
		keyGenAIRequestModel:  "claude-opus-4-7",
		keyGenAIResponseModel: "claude-opus-4-7",
		keyGenAIResponseID:    "req_1",
		keySBOProjectRoot:     "/srv/app",
		keySBOSessionID:       "sess-9",
		keySBOOrgID:           "org-x",
		keySBOUserEmail:       "dev@acme.com",
	}
	for k, want := range wantStr {
		if got := a[k].GetStringValue(); got != want {
			t.Errorf("span attr %s = %q, want %q", k, got, want)
		}
	}
	if a[keyGenAIUsageInputTok].GetIntValue() != 100 || a[keyGenAIUsageOutputTok].GetIntValue() != 50 {
		t.Errorf("usage tokens on wire wrong: in=%d out=%d",
			a[keyGenAIUsageInputTok].GetIntValue(), a[keyGenAIUsageOutputTok].GetIntValue())
	}
	if got := a[keySBOCostUSD].GetDoubleValue(); got != 0.05 {
		t.Errorf("sbo.cost.usd = %v, want 0.05", got)
	}
	if fr := a[keyGenAIFinishReasons].GetArrayValue().GetValues(); len(fr) != 1 || fr[0].GetStringValue() != "end_turn" {
		t.Errorf("finish_reasons on wire = %v, want [end_turn]", fr)
	}
	if ok.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_UNSET {
		t.Errorf("ok span status = %v, want UNSET", ok.GetStatus().GetCode())
	}

	errSpan := byName["chat gpt-x"]
	if errSpan == nil {
		t.Fatalf("missing error span 'chat gpt-x'")
	}
	if errSpan.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		t.Errorf("error span status = %v, want ERROR", errSpan.GetStatus().GetCode())
	}
	if got := pbAttrs(errSpan.GetAttributes())[errorTypeKey].GetStringValue(); got != "server_error" {
		t.Errorf("error.type on wire = %q, want server_error", got)
	}
}

func names(spans []*tracepb.Span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.GetName()
	}
	return out
}
