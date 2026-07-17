package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
)

// seedSpanContent inserts an obs_span_content row directly (there is no public
// writer yet — P2 wired spans/events/links but not content), so the
// dataset-from-traces join has something to snapshot.
func seedSpanContent(t *testing.T, s *Store, spanID, kind, content string) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO obs_span_content (span_id, kind, content, content_hash, time) VALUES (?, ?, ?, ?, ?)`,
		spanID, kind, content, "h_"+spanID+"_"+kind, ts(time.Now()))
	if err != nil {
		t.Fatalf("seed content: %v", err)
	}
}

func TestEvalPlane_DatasetRunScores(t *testing.T) {
	s := newDB(t)
	ctx := context.Background()
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	// Two LLM spans, one with content.
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "r", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "r", TraceID: "t1", Kind: span.KindAgent, Name: "run", StartedAt: start, EndedAt: start.Add(time.Second)},
		{SpanID: "llm1", TraceID: "t1", ParentSpanID: "r", Kind: span.KindLLM, Name: "chat", Model: "gpt-4o", Status: span.StatusOK, InputTokens: i64(100), OutputTokens: i64(20), CostUSD: f64(0.01), StartedAt: start, EndedAt: start.Add(1500 * time.Millisecond)},
		{SpanID: "llm2", TraceID: "t1", ParentSpanID: "r", Kind: span.KindLLM, Name: "chat2", Model: "gpt-4o", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	seedSpanContent(t, s, "llm1", "prompt", "what is 2+2?")
	seedSpanContent(t, s, "llm1", "response", `{"answer":4}`)

	// Dataset (idempotent) + items from traces.
	dsID, err := s.CreateDataset(ctx, "demo", "demo set")
	if err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	if again, err := s.CreateDataset(ctx, "demo", ""); err != nil || again != dsID {
		t.Fatalf("CreateDataset idempotent = %d,%v want %d", again, err, dsID)
	}
	n, err := s.AddDatasetItemsFromTraces(ctx, dsID, 100, true)
	if err != nil || n != 2 {
		t.Fatalf("AddDatasetItemsFromTraces = %d,%v want 2", n, err)
	}
	// Re-add is idempotent (no new items).
	if n2, err := s.AddDatasetItemsFromTraces(ctx, dsID, 100, true); err != nil || n2 != 0 {
		t.Fatalf("re-add = %d,%v want 0", n2, err)
	}

	samples, err := s.LoadSamples(ctx, dsID)
	if err != nil || len(samples) != 2 {
		t.Fatalf("LoadSamples = %d,%v want 2", len(samples), err)
	}
	var sawContent bool
	for _, sm := range samples {
		if sm.SpanID == "llm1" {
			sawContent = true
			if sm.Output != `{"answer":4}` || sm.Input != "what is 2+2?" {
				t.Errorf("llm1 sample content = in %q out %q", sm.Input, sm.Output)
			}
			if sm.DurationMS != 1500 || sm.Model != "gpt-4o" || sm.CostUSD != 0.01 {
				t.Errorf("llm1 facts = %+v", sm)
			}
		}
	}
	if !sawContent {
		t.Fatal("llm1 sample missing")
	}

	// Run lifecycle + scores.
	runID, err := s.CreateEvalRun(ctx, dsID, "r1", `[{"name":"json_valid"}]`)
	if err != nil {
		t.Fatalf("CreateEvalRun: %v", err)
	}
	if err := s.InsertScores(ctx, []ScoreRow{
		{RunID: &runID, SpanID: "llm1", Scorer: "json_valid", Score: 1, Passed: true, Source: "run"},
		{RunID: &runID, SpanID: "llm2", Scorer: "json_valid", Score: 0, Passed: false, Source: "run"},
	}); err != nil {
		t.Fatalf("InsertScores: %v", err)
	}
	if err := s.FinishEvalRun(ctx, runID, 2, 1, 0.5, "done"); err != nil {
		t.Fatalf("FinishEvalRun: %v", err)
	}
	runs, err := s.ListEvalRuns(ctx, dsID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListEvalRuns = %d,%v want 1", len(runs), err)
	}
	if runs[0].Total != 2 || runs[0].Passed != 1 || runs[0].MeanScore != 0.5 || runs[0].Status != "done" {
		t.Errorf("run summary = %+v", runs[0])
	}

	// Online score (run_id NULL).
	if err := s.InsertScores(ctx, []ScoreRow{{SpanID: "llm1", Scorer: "status_ok", Score: 1, Passed: true, Source: "online"}}); err != nil {
		t.Fatalf("InsertScores online: %v", err)
	}
	var onlineCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM obs_eval_scores WHERE source='online' AND run_id IS NULL`).Scan(&onlineCount); err != nil || onlineCount != 1 {
		t.Fatalf("online score count = %d,%v want 1", onlineCount, err)
	}

	// ListDatasets.
	dss, err := s.ListDatasets(ctx)
	if err != nil || len(dss) != 1 || dss[0].ItemCount != 2 {
		t.Fatalf("ListDatasets = %+v,%v", dss, err)
	}
}

// TestEvalReads_RunsAndScores covers the dashboard read seams
// (ListAllEvalRuns / GetEvalRun / LoadRunScores) the Evals comparison UI
// serves from: two runs of one dataset, newest-first ordering with the dataset
// name joined, per-run score load aligned item-then-scorer, and that
// online-sampled scores (run_id NULL) are excluded from a run's scores.
func TestEvalReads_RunsAndScores(t *testing.T) {
	s := newDB(t)
	ctx := context.Background()
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, RootSpanID: "r", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
		{SpanID: "llm2", TraceID: "t1", Kind: span.KindLLM, Name: "chat2", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	seedSpanContent(t, s, "llm1", "response", `{"ok":true}`)
	dsID, _ := s.CreateDataset(ctx, "demo", "")
	if _, err := s.AddDatasetItemsFromTraces(ctx, dsID, 100, true); err != nil {
		t.Fatalf("AddDatasetItemsFromTraces: %v", err)
	}

	// Two runs on the same dataset.
	run1, _ := s.CreateEvalRun(ctx, dsID, "baseline", `[{"name":"json_valid"}]`)
	if err := s.InsertScores(ctx, []ScoreRow{
		{RunID: &run1, ItemID: itemIDFor(t, s, "llm1"), SpanID: "llm1", Scorer: "json_valid", Score: 1, Passed: true, Source: "run"},
		{RunID: &run1, ItemID: itemIDFor(t, s, "llm2"), SpanID: "llm2", Scorer: "json_valid", Score: 0, Passed: false, Rationale: "not json", Source: "run"},
	}); err != nil {
		t.Fatalf("InsertScores run1: %v", err)
	}
	_ = s.FinishEvalRun(ctx, run1, 2, 1, 0.5, "done")

	run2, _ := s.CreateEvalRun(ctx, dsID, "after-fix", `[{"name":"json_valid"}]`)
	if err := s.InsertScores(ctx, []ScoreRow{
		{RunID: &run2, ItemID: itemIDFor(t, s, "llm1"), SpanID: "llm1", Scorer: "json_valid", Score: 1, Passed: true, Source: "run"},
		{RunID: &run2, ItemID: itemIDFor(t, s, "llm2"), SpanID: "llm2", Scorer: "json_valid", Score: 1, Passed: true, Source: "run"},
	}); err != nil {
		t.Fatalf("InsertScores run2: %v", err)
	}
	_ = s.FinishEvalRun(ctx, run2, 2, 2, 1.0, "done")

	// An online score must never surface in a run's scores.
	if err := s.InsertScores(ctx, []ScoreRow{{SpanID: "llm1", Scorer: "status_ok", Score: 1, Passed: true, Source: "online"}}); err != nil {
		t.Fatalf("InsertScores online: %v", err)
	}

	// ListAllEvalRuns: newest-first, dataset name joined.
	all, err := s.ListAllEvalRuns(ctx, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAllEvalRuns = %d,%v want 2", len(all), err)
	}
	if all[0].ID != run2 || all[0].DatasetName != "demo" || all[0].Passed != 2 {
		t.Errorf("newest run row = %+v", all[0])
	}

	// GetEvalRun: found + not-found.
	got, found, err := s.GetEvalRun(ctx, run1)
	if err != nil || !found || got.Name != "baseline" || got.DatasetName != "demo" || got.MeanScore != 0.5 {
		t.Errorf("GetEvalRun(run1) = %+v found=%v err=%v", got, found, err)
	}
	if _, found, _ := s.GetEvalRun(ctx, 9999); found {
		t.Error("GetEvalRun(9999) found=true, want false")
	}

	// LoadRunScores: only this run's scores, item-then-scorer order, with item context.
	sc1, err := s.LoadRunScores(ctx, run1)
	if err != nil || len(sc1) != 2 {
		t.Fatalf("LoadRunScores(run1) = %d,%v want 2", len(sc1), err)
	}
	for _, r := range sc1 {
		if r.ItemID == 0 || r.TraceID != "t1" {
			t.Errorf("score row missing item context: %+v", r)
		}
		if r.SpanID == "llm2" && (r.Passed || r.Rationale != "not json") {
			t.Errorf("llm2 score = %+v", r)
		}
	}
}

// itemIDFor resolves the dataset item id for a span (test helper), returning
// it as a *int64 to match ScoreRow.ItemID.
func itemIDFor(t *testing.T, s *Store, spanID string) *int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(context.Background(), `SELECT id FROM obs_dataset_items WHERE span_id = ?`, spanID).Scan(&id); err != nil {
		t.Fatalf("itemIDFor(%s): %v", spanID, err)
	}
	return &id
}

// TestAddDatasetItems_MetadataOnly proves the ContentGate path: with
// includeContent=false, raw input/output are NOT persisted but the content_hash
// still is (computed from the source content).
func TestAddDatasetItems_MetadataOnly(t *testing.T) {
	s := newDB(t)
	ctx := context.Background()
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := s.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, Status: span.StatusOK, StartedAt: start}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := s.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	seedSpanContent(t, s, "llm1", "response", "secret answer")

	dsID, _ := s.CreateDataset(ctx, "meta", "")
	if _, err := s.AddDatasetItemsFromTraces(ctx, dsID, 100, false); err != nil {
		t.Fatalf("AddDatasetItemsFromTraces: %v", err)
	}
	samples, _ := s.LoadSamples(ctx, dsID)
	if len(samples) != 1 || samples[0].Output != "" {
		t.Fatalf("metadata-only should drop raw output, got %q", samples[0].Output)
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT content_hash FROM obs_dataset_items WHERE span_id='llm1'`).Scan(&hash); err != nil || hash == "" {
		t.Fatalf("content_hash should persist even when raw dropped, got %q (%v)", hash, err)
	}
}
