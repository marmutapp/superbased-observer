package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

// TestLoadAdmissionReplaySamples pins the §9 simulate corpus loader: it returns
// only prompt bodies that retained raw content (content-gated-off rows and
// non-prompt kinds are skipped), newest-first, capped at limit.
func TestLoadAdmissionReplaySamples(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	start := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "s1", StartedAt: start}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{{SpanID: "s1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", StartedAt: start}}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	if err := s.InsertSpanContent(ctx, []span.SpanContent{
		{SpanID: "s1", TraceID: "t1", Kind: span.ContentPrompt, ContentHash: "h1", Raw: "book me a flight", Time: start},
		{SpanID: "s1", TraceID: "t1", Kind: span.ContentPrompt, ContentHash: "h2", Raw: "write my novel", Time: start.Add(time.Minute)},
		// gated-off prompt (no raw content) — not replayable.
		{SpanID: "s1", TraceID: "t1", Kind: span.ContentPrompt, ContentHash: "h3", Raw: "", Time: start.Add(2 * time.Minute)},
		// a response body — not a request, must be skipped.
		{SpanID: "s1", TraceID: "t1", Kind: span.ContentResponse, ContentHash: "h4", Raw: "here you go", Time: start},
	}); err != nil {
		t.Fatalf("InsertSpanContent: %v", err)
	}

	got, err := s.LoadAdmissionReplaySamples(ctx, 100)
	if err != nil {
		t.Fatalf("LoadAdmissionReplaySamples: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2 (only content-bearing prompts)", len(got))
	}
	// Newest-first: the later prompt comes first.
	if got[0].Text != "write my novel" || got[1].Text != "book me a flight" {
		t.Errorf("order/content wrong: %+v", got)
	}
	for _, sm := range got {
		if sm.Text == "" {
			t.Errorf("returned an empty-text sample: %+v", sm)
		}
	}

	// Limit caps the corpus.
	one, err := s.LoadAdmissionReplaySamples(ctx, 1)
	if err != nil {
		t.Fatalf("limit load: %v", err)
	}
	if len(one) != 1 {
		t.Errorf("limit=1 returned %d", len(one))
	}
}
