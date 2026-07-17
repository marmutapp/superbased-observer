package dashboard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedBenchmarkRun inserts one project + session with billed api_turns (two
// success turns + one excluded 400 turn), a run over a two-config / one-task
// spec, one attempt bound to the session, and a passing score. Mirrors the
// store test's fixture so the handler exercises the real correlation shape.
func seedBenchmarkRun(t *testing.T, s *Server) string {
	t.Helper()
	ctx := context.Background()
	st := store.New(s.opts.DB)
	spec := benchmark.Spec{
		Name:    "corpus-v1",
		Repeats: 1,
		Analysis: benchmark.Analysis{
			BaselineConfig: "codex-gpt5", NonInferiorityMargin: 0.1, MinSample: 1,
		},
		Tasks: []benchmark.Task{{
			ID: "t1", Repo: "https://example.com/r.git", Prompt: "do it",
			Success: benchmark.Success{Scorer: "tests_pass", Cmd: "go test ./..."},
		}},
		Configs: []benchmark.Config{
			{ID: "codex-gpt5", Harness: "codex", Model: "gpt-5.6-sol"},
			{ID: "claude-sonnet", Harness: "claude-code", Model: "claude-sonnet-4-5"},
		},
	}
	specJSON, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.opts.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec: %v", err)
		}
	}
	mustExec(`INSERT INTO projects (id, root_path, created_at) VALUES (91, '/tmp/ws-a', ?)`, time.Now().UTC().Format(time.RFC3339))
	mustExec(`INSERT INTO sessions (id, project_id, tool, model, started_at) VALUES ('bsess-a', 91, 'codex', 'gpt-5.6-sol', '2026-07-12T10:00:00Z')`)
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES ('bsess-a', '2026-07-12T10:00:01Z', 'openai', 'gpt-5.6-sol', 6000, 100, 3800, 0.035, NULL)`)
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES ('bsess-a', '2026-07-12T10:00:05Z', 'openai', 'gpt-5.6-sol', 1200, 80, 9000, 0.012, NULL)`)
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES ('bsess-a', '2026-07-12T10:00:09Z', 'openai', 'gpt-5.6-sol', 0, 0, 0, 0.0, 400)`)

	runID := "bench-corpus-v1-1"
	finished := time.Now().UTC()
	if err := st.InsertBenchmarkRun(ctx, benchmark.RunRecord{
		RunID: runID, SpecName: spec.Name, SpecHash: spec.SpecHash(), SpecJSON: specJSON,
		StartedAt: finished.Add(-time.Minute), FinishedAt: finished, Status: "completed",
		PlannedCells: 2, CompletedCells: 2, SpendUSD: 0.047, JudgeSpendUSD: 0,
		PricingSnapshotJSON: `{"hash":"abc","entries":[]}`,
	}); err != nil {
		t.Fatalf("InsertBenchmarkRun: %v", err)
	}
	exit := 0
	aid, err := st.InsertBenchmarkAttempt(ctx, benchmark.AttemptRecord{
		RunID: runID, TaskID: "t1", ConfigID: "codex-gpt5", Harness: "codex",
		ModelRequested: "gpt-5.6-sol", RepeatIdx: 0, WorkspacePath: "/tmp/ws-a",
		WallMS: 36900, ExitCode: &exit, Status: benchmark.StatusOK, Turns: 2,
		StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
	})
	if err != nil {
		t.Fatalf("InsertBenchmarkAttempt: %v", err)
	}
	if err := st.InsertBenchmarkSessionMembers(ctx, []benchmark.SessionMember{{
		AttemptID: aid, RunID: runID, SessionID: "bsess-a", Role: benchmark.RolePrimary, ModelReturned: "gpt-5.6-sol",
	}}); err != nil {
		t.Fatalf("InsertBenchmarkSessionMembers: %v", err)
	}
	if err := st.InsertBenchmarkScores(ctx, []benchmark.ScoreRecord{{
		AttemptID: aid, RunID: runID, Scorer: "tests_pass", Score: 1, Passed: true,
	}}); err != nil {
		t.Fatalf("InsertBenchmarkScores: %v", err)
	}
	return runID
}

func benchmarkGET(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal %s: %v (%s)", path, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// TestAPIBenchmarksEmpty pins the honest empty state: an install with no runs
// returns runs:[] (never null) so the page can render the "run observer
// benchmark run" empty state instead of a crash.
func TestAPIBenchmarksEmpty(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	code, out := benchmarkGET(t, s, "/api/benchmarks")
	if code != 200 {
		t.Fatalf("GET /api/benchmarks = %d", code)
	}
	runs, ok := out["runs"].([]any)
	if !ok {
		t.Fatalf("runs not an array: %T (%v)", out["runs"], out["runs"])
	}
	if len(runs) != 0 {
		t.Errorf("want 0 runs, got %d", len(runs))
	}
	if out["total"].(float64) != 0 {
		t.Errorf("total = %v, want 0", out["total"])
	}
}

// TestAPIBenchmarksList pins the sanitized run-list summary: lifecycle, spend,
// and the config-matrix shape parsed from the stored spec (harnesses/models,
// no prompts/paths).
func TestAPIBenchmarksList(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	runID := seedBenchmarkRun(t, s)

	code, out := benchmarkGET(t, s, "/api/benchmarks")
	if code != 200 {
		t.Fatalf("GET /api/benchmarks = %d", code)
	}
	runs := out["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	row := runs[0].(map[string]any)
	if row["run_id"] != runID {
		t.Errorf("run_id = %v", row["run_id"])
	}
	if row["status"] != "completed" {
		t.Errorf("status = %v", row["status"])
	}
	if row["configs"].(float64) != 2 || row["tasks"].(float64) != 1 {
		t.Errorf("matrix shape = %v configs / %v tasks", row["configs"], row["tasks"])
	}
	harnesses := row["harnesses"].([]any)
	if len(harnesses) != 2 {
		t.Errorf("harnesses = %v", harnesses)
	}
	// The stored prompt must never appear in a list response.
	if body, _ := json.Marshal(out); jsonContains(body, "do it") {
		t.Error("list response leaked the task prompt")
	}
}

// TestAPIBenchmarkDetail pins the run-detail endpoint: the verdicts come from
// ComputeReport (the CLI's exact math), the per-task grid + cost dots are
// present, and the excluded 400-turn keeps cost at the two-success-turn sum.
func TestAPIBenchmarkDetail(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	runID := seedBenchmarkRun(t, s)

	code, out := benchmarkGET(t, s, "/api/benchmarks/"+runID)
	if code != 200 {
		t.Fatalf("GET detail = %d: %v", code, out)
	}
	if out["run_id"] != runID {
		t.Errorf("run_id = %v", out["run_id"])
	}
	if out["baseline_config"] != "codex-gpt5" {
		t.Errorf("baseline = %v", out["baseline_config"])
	}
	configs := out["configs"].([]any)
	if len(configs) != 2 {
		t.Fatalf("want 2 config rows (all reported), got %d", len(configs))
	}
	// codex-gpt5 executed one passing attempt; success rate 1.0, cost = 0.047.
	var codex map[string]any
	for _, c := range configs {
		cc := c.(map[string]any)
		if cc["config_id"] == "codex-gpt5" {
			codex = cc
		}
	}
	if codex == nil {
		t.Fatal("codex-gpt5 config row missing")
	}
	if codex["n_executed"].(float64) != 1 || codex["n_passed"].(float64) != 1 {
		t.Errorf("codex N = %v executed / %v passed", codex["n_executed"], codex["n_passed"])
	}
	if got := codex["total_spend_usd"].(float64); got < 0.046 || got > 0.048 {
		t.Errorf("codex spend = %v, want ~0.047 (400 turn excluded)", got)
	}
	// The zero-attempt config still appears (nothing hidden).
	comparisons := out["comparisons"].([]any)
	if len(comparisons) != 1 {
		t.Errorf("want 1 comparison vs baseline, got %d", len(comparisons))
	}
	// Per-task grid present.
	tasks := out["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("want 1 task row, got %d", len(tasks))
	}
	// Cost dots present for the executed config.
	dots := out["cost_dots"].(map[string]any)
	if _, ok := dots["codex-gpt5"]; !ok {
		t.Errorf("cost_dots missing codex-gpt5: %v", dots)
	}
	if out["price_disclaimer"] != benchmark.PriceDisclaimer {
		t.Errorf("price_disclaimer = %v", out["price_disclaimer"])
	}
}

// TestAPIBenchmarkExport pins the export passthrough: the same canonical card
// the CLI emits, carrying the schema + disclaimer.
func TestAPIBenchmarkExport(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	runID := seedBenchmarkRun(t, s)

	code, out := benchmarkGET(t, s, "/api/benchmarks/"+runID+"/export")
	if code != 200 {
		t.Fatalf("GET export = %d", code)
	}
	if out["schema"] != benchmark.ExportSchema {
		t.Errorf("schema = %v, want %v", out["schema"], benchmark.ExportSchema)
	}
	if out["price_disclaimer"] != benchmark.PriceDisclaimer {
		t.Errorf("disclaimer = %v", out["price_disclaimer"])
	}
}

// TestAPIBenchmarkDetailNotFound + traversal guard.
func TestAPIBenchmarkDetailNotFound(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	if code, _ := benchmarkGET(t, s, "/api/benchmarks/nope"); code != 404 {
		t.Errorf("unknown run = %d, want 404", code)
	}
	code := func(path string) int {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := code("/api/benchmarks/..%2f..%2fetc/passwd"); c == 200 {
		t.Errorf("traversal-ish path returned 200")
	}
}

func jsonContains(body []byte, needle string) bool {
	return strings.Contains(string(body), needle)
}
