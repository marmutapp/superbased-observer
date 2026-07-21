package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
)

// seedBenchmarkFixture inserts a project, two sessions with billed api_turns
// (one success, one with a 400 error turn that must be excluded from cost), and
// returns the store. Mirrors the real correlation shape: attempt → session
// members → api_turns.
func seedBenchmarkRun(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`INSERT INTO projects (id, root_path, created_at) VALUES (11, '/tmp/ws-a', ?)`, timestamp(time.Now()))
	mustExec(`INSERT INTO sessions (id, project_id, tool, model, started_at) VALUES ('sess-a', 11, 'codex', 'gpt-5.6-sol', '2026-07-12T10:00:00Z')`)
	// two success turns + one 400 error turn (must be excluded from cost)
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES ('sess-a', '2026-07-12T10:00:01Z', 'openai', 'gpt-5.6-sol', 6000, 100, 3800, 0.035, NULL)`)
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES ('sess-a', '2026-07-12T10:00:05Z', 'openai', 'gpt-5.6-sol', 1200, 80, 9000, 0.012, NULL)`)
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES ('sess-a', '2026-07-12T10:00:09Z', 'openai', 'gpt-5.6-sol', 0, 0, 0, 0.0, 400)`)

	runID := "run-001"
	if err := s.InsertBenchmarkRun(ctx, benchmark.RunRecord{
		RunID: runID, SpecName: "corpus-v1", SpecHash: "h", SpecJSON: "{}",
		StartedAt: time.Now().UTC(), Status: "running", PlannedCells: 1,
	}); err != nil {
		t.Fatalf("InsertBenchmarkRun: %v", err)
	}
	exit := 0
	aid, err := s.InsertBenchmarkAttempt(ctx, benchmark.AttemptRecord{
		RunID: runID, TaskID: "t1", ConfigID: "codex-gpt5", Harness: "codex",
		ModelRequested: "gpt-5.6-sol", RepeatIdx: 0, WorkspacePath: "/tmp/ws-a",
		WallMS: 36900, ExitCode: &exit, Status: benchmark.StatusOK, Turns: 2,
		StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("InsertBenchmarkAttempt: %v", err)
	}
	if err := s.InsertBenchmarkSessionMembers(ctx, []benchmark.SessionMember{{
		AttemptID: aid, RunID: runID, SessionID: "sess-a", Role: benchmark.RolePrimary, ModelReturned: "gpt-5.6-sol",
	}}); err != nil {
		t.Fatalf("InsertBenchmarkSessionMembers: %v", err)
	}
	if err := s.InsertBenchmarkScores(ctx, []benchmark.ScoreRecord{{
		AttemptID: aid, RunID: runID, Scorer: "tests_pass", Score: 1, Passed: true,
	}}); err != nil {
		t.Fatalf("InsertBenchmarkScores: %v", err)
	}
	return runID
}

func TestBenchmarkRunRoundTripAndFacts(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	runID := seedBenchmarkRun(t, s)

	// Run header round-trip.
	run, ok, err := s.LoadBenchmarkRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("LoadBenchmarkRun: ok=%v err=%v", ok, err)
	}
	if run.SpecName != "corpus-v1" || run.PlannedCells != 1 {
		t.Fatalf("run header mismatch: %+v", run)
	}

	// Facts: cost excludes the 400 turn (0.035 + 0.012 = 0.047), passed=true.
	facts, err := s.LoadBenchmarkFacts(ctx, runID)
	if err != nil {
		t.Fatalf("LoadBenchmarkFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if !f.Scored || !f.Passed {
		t.Errorf("expected scored+passed, got scored=%v passed=%v", f.Scored, f.Passed)
	}
	if f.CostUSD < 0.046 || f.CostUSD > 0.048 {
		t.Errorf("cost should exclude the 400 turn (want ~0.047), got %v", f.CostUSD)
	}
	if f.InputTokens != 7200 { // 6000 + 1200, 400-turn excluded
		t.Errorf("input tokens = %d, want 7200", f.InputTokens)
	}
	if f.Status != benchmark.StatusOK {
		t.Errorf("status = %q", f.Status)
	}
}

func TestBenchmarkSessionCorrelation(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedBenchmarkRun(t, s)

	fact, ok, err := s.LoadSessionCorrelation(ctx, "sess-a")
	if err != nil || !ok {
		t.Fatalf("LoadSessionCorrelation: ok=%v err=%v", ok, err)
	}
	if fact.Tool != "codex" || fact.RootPath != "/tmp/ws-a" {
		t.Errorf("correlation mismatch: %+v", fact)
	}
	if _, ok, _ := s.LoadSessionCorrelation(ctx, "no-such"); ok {
		t.Error("expected ok=false for unknown session")
	}

	bill, err := s.LoadSessionBilling(ctx, "sess-a")
	if err != nil {
		t.Fatalf("LoadSessionBilling: %v", err)
	}
	if bill.Turns != 2 { // 400 turn excluded
		t.Errorf("billing turns = %d, want 2 (400 excluded)", bill.Turns)
	}
}

func TestPruneBenchmarkRowsAndDelete(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	runID := seedBenchmarkRun(t, s)

	// retention_days <= 0 is a no-op.
	n, err := s.PruneBenchmarkRows(ctx, 0)
	if err != nil || n != 0 {
		t.Fatalf("prune(0): n=%d err=%v", n, err)
	}

	// Age the run beyond the horizon then prune.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE benchmark_runs SET started_at = ? WHERE run_id = ?`,
		"2020-01-01T00:00:00Z", runID); err != nil {
		t.Fatalf("age run: %v", err)
	}
	n, err = s.PruneBenchmarkRows(ctx, 30)
	if err != nil || n != 1 {
		t.Fatalf("prune(30): n=%d err=%v", n, err)
	}
	if _, ok, _ := s.LoadBenchmarkRun(ctx, runID); ok {
		t.Error("run should be pruned")
	}
	// Cascade check: no orphan attempts/scores/members.
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM benchmark_attempts WHERE run_id = ?`, runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("attempts not cascaded: %d remain", count)
	}
}
