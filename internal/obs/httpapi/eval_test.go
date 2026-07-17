package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// seedEvalRun seeds one dataset + one finished run with two scores, returning
// the run id. It reuses the package's newServer store so the eval endpoints
// have something to serve.
func seedEvalRun(t *testing.T, st *obsstore.Store) int64 {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertTrace(ctx, span.Trace{TraceID: "t1", Source: span.SourceOTLPTrace, Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	if err := st.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: "llm1", TraceID: "t1", Kind: span.KindLLM, Name: "chat", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
		{SpanID: "llm2", TraceID: "t1", Kind: span.KindLLM, Name: "chat2", Status: span.StatusOK, StartedAt: start, EndedAt: start.Add(time.Second)},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
	dsID, err := st.CreateDataset(ctx, "demo", "demo set")
	if err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	if _, err := st.AddDatasetItemsFromTraces(ctx, dsID, 100, true); err != nil {
		t.Fatalf("AddDatasetItemsFromTraces: %v", err)
	}
	runID, err := st.CreateEvalRun(ctx, dsID, "baseline", `[{"name":"json_valid"}]`)
	if err != nil {
		t.Fatalf("CreateEvalRun: %v", err)
	}
	if err := st.InsertScores(ctx, []obsstore.ScoreRow{
		{RunID: &runID, SpanID: "llm1", Scorer: "json_valid", Score: 1, Passed: true, Source: "run"},
		{RunID: &runID, SpanID: "llm2", Scorer: "json_valid", Score: 0, Passed: false, Rationale: "bad", Source: "run"},
	}); err != nil {
		t.Fatalf("InsertScores: %v", err)
	}
	if err := st.FinishEvalRun(ctx, runID, 2, 1, 0.5, "done"); err != nil {
		t.Fatalf("FinishEvalRun: %v", err)
	}
	return runID
}

func TestAPI_EvalDatasetsAndRuns(t *testing.T) {
	srv, st := newServer(t)
	runID := seedEvalRun(t, st)

	var ds struct {
		Datasets []obsstore.DatasetRow `json:"datasets"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/eval/datasets", &ds); code != 200 {
		t.Fatalf("/eval/datasets = %d, want 200", code)
	}
	if len(ds.Datasets) != 1 || ds.Datasets[0].Name != "demo" || ds.Datasets[0].ItemCount != 2 {
		t.Errorf("datasets = %+v", ds.Datasets)
	}

	var runs struct {
		Runs []obsstore.EvalRunListRow `json:"runs"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/eval/runs", &runs); code != 200 {
		t.Fatalf("/eval/runs = %d, want 200", code)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].ID != runID || runs.Runs[0].DatasetName != "demo" {
		t.Errorf("runs = %+v", runs.Runs)
	}
}

func TestAPI_EvalRunDetail(t *testing.T) {
	srv, st := newServer(t)
	runID := seedEvalRun(t, st)

	var detail struct {
		Run    obsstore.EvalRunListRow `json:"run"`
		Scores []obsstore.RunScoreRow  `json:"scores"`
	}
	if code := getJSON(t, srv.URL+"/api/obs/eval/run/"+itoa(runID), &detail); code != 200 {
		t.Fatalf("/eval/run/%d = %d, want 200", runID, code)
	}
	if detail.Run.Name != "baseline" || detail.Run.MeanScore != 0.5 {
		t.Errorf("run = %+v", detail.Run)
	}
	if len(detail.Scores) != 2 {
		t.Fatalf("scores len = %d, want 2", len(detail.Scores))
	}
	var sawFail bool
	for _, sc := range detail.Scores {
		if sc.SpanID == "llm2" {
			sawFail = true
			if sc.Passed || sc.Rationale != "bad" {
				t.Errorf("llm2 score = %+v", sc)
			}
		}
	}
	if !sawFail {
		t.Error("llm2 score missing")
	}

	// Unknown run → 404; non-numeric → 400.
	if code := getJSON(t, srv.URL+"/api/obs/eval/run/9999", nil); code != http.StatusNotFound {
		t.Errorf("/eval/run/9999 = %d, want 404", code)
	}
	if code := getJSON(t, srv.URL+"/api/obs/eval/run/abc", nil); code != http.StatusBadRequest {
		t.Errorf("/eval/run/abc = %d, want 400", code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
