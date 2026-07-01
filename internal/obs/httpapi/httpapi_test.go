package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

func i64(v int64) *int64 { return &v }

// fakeEnricher is a stand-in ProxyEnricher: it returns Found for exactly the
// request_ids it was seeded with, mirroring the host's pull-only contract
// without touching api_turns.
type fakeEnricher struct{ byReq map[string]obs.Enrichment }

func (f fakeEnricher) EnrichByRequestID(_ context.Context, reqID string) (obs.Enrichment, error) {
	if e, ok := f.byReq[reqID]; ok {
		return e, nil
	}
	return obs.Enrichment{}, nil
}

func newServer(t *testing.T) (*httptest.Server, *obsstore.Store) {
	return newServerWith(t, nil)
}

func newServerWith(t *testing.T, enricher obs.ProxyEnricher) (*httptest.Server, *obsstore.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	mux := http.NewServeMux()
	for _, r := range New(st, enricher, nil).Routes() {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

func seedTrace(t *testing.T, st *obsstore.Store) {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertTrace(ctx, span.Trace{
		TraceID: "t1", SessionID: "s1", Source: span.SourceOTLPTrace,
		RootSpanID: "root", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := st.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "root", TraceID: "t1", Kind: span.KindAgent, Name: "run", StartedAt: start, EndedAt: start.Add(2 * time.Second)},
		{
			SpanID: "llm1", TraceID: "t1", ParentSpanID: "root", Kind: span.KindLLM, Name: "chat",
			Model: "gpt-4o", InputTokens: i64(100), OutputTokens: i64(20), RequestID: "chatcmpl-x",
			StartedAt: start, EndedAt: start.Add(time.Second),
		},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func TestAPI_EnabledAndTraces(t *testing.T) {
	srv, st := newServer(t)
	seedTrace(t, st)

	var en struct {
		Enabled bool `json:"enabled"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/enabled", &en); code != 200 || !en.Enabled {
		t.Errorf("/enabled = %d %v, want 200 true", code, en.Enabled)
	}

	var list struct {
		Traces []obsstore.TraceListRow `json:"traces"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/traces", &list); code != 200 {
		t.Fatalf("/traces = %d, want 200", code)
	}
	if len(list.Traces) != 1 {
		t.Fatalf("traces len = %d, want 1", len(list.Traces))
	}
	tr := list.Traces[0]
	if tr.TraceID != "t1" || tr.RootName != "run" || tr.SpanCount != 2 || tr.DurationMS != 2000 {
		t.Errorf("trace row = %+v", tr)
	}
}

func TestAPI_TraceDetailAndNotFound(t *testing.T) {
	srv, st := newServer(t)
	seedTrace(t, st)

	var d obsstore.TraceDetail
	if code := getJSON(t, srv.URL+"/api/obs/trace/t1", &d); code != 200 {
		t.Fatalf("/trace/t1 = %d, want 200", code)
	}
	if len(d.Spans) != 2 {
		t.Fatalf("detail spans = %d, want 2", len(d.Spans))
	}
	// Spans ordered by start; both share start here, so just check presence.
	var sawLLM bool
	for _, s := range d.Spans {
		if s.SpanID == "llm1" {
			sawLLM = true
			if s.Kind != "llm" || s.Model != "gpt-4o" || s.InputTokens == nil || *s.InputTokens != 100 {
				t.Errorf("llm span = %+v", s)
			}
		}
	}
	if !sawLLM {
		t.Error("llm1 span missing from detail")
	}

	if code := getJSON(t, srv.URL+"/api/obs/trace/nope", nil); code != http.StatusNotFound {
		t.Errorf("/trace/nope = %d, want 404", code)
	}
}

// TestAPI_TraceDetailEnrichment proves the §9/P6 wedge: an LLM span carrying a
// request_id the proxy also saw is enriched pull-only with proxy-exact
// cost/cache/routing; a span without a matching request_id stays bare.
func TestAPI_TraceDetailEnrichment(t *testing.T) {
	enr := fakeEnricher{byReq: map[string]obs.Enrichment{
		"chatcmpl-x": {
			Found: true, Provider: "openai", Model: "gpt-4o",
			InputTokens: 100, OutputTokens: 20, CacheReadTokens: 64, CacheCreationTokens: 16,
			CostUSD: 0.0123, RoutingReason: "→ gpt-4o-mini (downshift)",
		},
	}}
	srv, st := newServerWith(t, enr)
	seedTrace(t, st)

	var d struct {
		obsstore.TraceDetail
		Enrichments map[string]obs.Enrichment `json:"enrichments"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/trace/t1", &d); code != 200 {
		t.Fatalf("/trace/t1 = %d, want 200", code)
	}
	got, ok := d.Enrichments["llm1"]
	if !ok {
		t.Fatalf("enrichments missing llm1: %+v", d.Enrichments)
	}
	if !got.Found || got.CostUSD != 0.0123 || got.CacheReadTokens != 64 || got.RoutingReason != "→ gpt-4o-mini (downshift)" {
		t.Errorf("enrichment = %+v", got)
	}
	// The root (agent) span has no request_id → never enriched.
	if _, ok := d.Enrichments["root"]; ok {
		t.Errorf("root span unexpectedly enriched")
	}
}

// TestAPI_TraceDetailNilEnricher confirms a node with no proxy wiring serves
// bare detail (enrichments omitted) and never panics.
func TestAPI_TraceDetailNilEnricher(t *testing.T) {
	srv, st := newServer(t) // nil enricher
	seedTrace(t, st)
	var d struct {
		Enrichments map[string]obs.Enrichment `json:"enrichments"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/trace/t1", &d); code != 200 {
		t.Fatalf("/trace/t1 = %d, want 200", code)
	}
	if d.Enrichments != nil {
		t.Errorf("enrichments = %+v, want nil/omitted", d.Enrichments)
	}
}
