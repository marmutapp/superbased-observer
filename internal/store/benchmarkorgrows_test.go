package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
)

// seedBenchmarkOrgRun inserts a run with two configs (two attempts), real
// spec_json (so RepoPathsJSON/TaskPrompt have something to recover), and a
// scored+rationale'd attempt, wired to a billed session — mirrors
// seedBenchmarkRun (benchmark_test.go) but adds the RAW-field surface
// SelectBenchmarkOrgRows needs that the CLI-report seed doesn't exercise.
func seedBenchmarkOrgRun(t *testing.T, s *Store, runID string, startedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	spec := benchmark.Spec{
		Name: "corpus-org-v1",
		Tasks: []benchmark.Task{
			{ID: "t1", Repo: "github.com/marmutapp/superbased-observer", Prompt: "Fix the flaky test in internal/store."},
		},
		Configs: []benchmark.Config{
			{ID: "codex-gpt5", Harness: "codex", Model: "gpt-5.6-sol"},
			{ID: "claude-sonnet", Harness: "claude-code", Model: "sonnet-5"},
		},
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	sessA := runID + "-sess-a"
	sessB := runID + "-sess-b"
	mustExec(`INSERT INTO projects (root_path, created_at) VALUES (?, ?)`, "/tmp/ws-"+runID, timestamp(time.Now()))
	mustExec(`INSERT INTO sessions (id, project_id, tool, model, started_at) VALUES (?, (SELECT id FROM projects WHERE root_path = ?), 'codex', 'gpt-5.6-sol', ?)`,
		sessA, "/tmp/ws-"+runID, timestamp(startedAt))
	mustExec(`INSERT INTO sessions (id, project_id, tool, model, started_at) VALUES (?, (SELECT id FROM projects WHERE root_path = ?), 'claude-code', 'sonnet-5', ?)`,
		sessB, "/tmp/ws-"+runID, timestamp(startedAt))
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES (?, ?, 'openai', 'gpt-5.6-sol', 5000, 200, 1000, 0.05, NULL)`, sessA, timestamp(startedAt.Add(time.Second)))
	mustExec(`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
	          VALUES (?, ?, 'anthropic', 'sonnet-5', 4000, 150, 2000, 0.03, NULL)`, sessB, timestamp(startedAt.Add(time.Second)))

	if err := s.InsertBenchmarkRun(ctx, benchmark.RunRecord{
		RunID: runID, SpecName: spec.Name, SpecHash: "h-" + runID, SpecJSON: string(specJSON),
		StartedAt: startedAt, FinishedAt: startedAt.Add(2 * time.Minute), Status: "completed", PlannedCells: 2,
	}); err != nil {
		t.Fatalf("InsertBenchmarkRun: %v", err)
	}

	exit0 := 0
	aidA, err := s.InsertBenchmarkAttempt(ctx, benchmark.AttemptRecord{
		RunID: runID, TaskID: "t1", ConfigID: "codex-gpt5", Harness: "codex",
		ModelRequested: "gpt-5.6-sol", RepeatIdx: 0, WorkspacePath: "/tmp/ws-" + runID,
		WallMS: 12000, ExitCode: &exit0, Status: benchmark.StatusOK, Turns: 3,
		FinalAnswerExcerpt: "Patched the flaky assertion.",
		StartedAt:          startedAt, FinishedAt: startedAt.Add(90 * time.Second),
	})
	if err != nil {
		t.Fatalf("InsertBenchmarkAttempt A: %v", err)
	}
	aidB, err := s.InsertBenchmarkAttempt(ctx, benchmark.AttemptRecord{
		RunID: runID, TaskID: "t1", ConfigID: "claude-sonnet", Harness: "claude-code",
		ModelRequested: "sonnet-5", RepeatIdx: 0, WorkspacePath: "/tmp/ws-" + runID,
		WallMS: 8000, ExitCode: &exit0, Status: benchmark.StatusModelFail, Turns: 2,
		FinalAnswerExcerpt: "Could not reproduce.",
		StartedAt:          startedAt, FinishedAt: startedAt.Add(60 * time.Second),
	})
	if err != nil {
		t.Fatalf("InsertBenchmarkAttempt B: %v", err)
	}

	if err := s.InsertBenchmarkSessionMembers(ctx, []benchmark.SessionMember{
		{AttemptID: aidA, RunID: runID, SessionID: sessA, Role: benchmark.RolePrimary, ModelReturned: "gpt-5.6-sol"},
		{AttemptID: aidB, RunID: runID, SessionID: sessB, Role: benchmark.RolePrimary, ModelReturned: "sonnet-5"},
	}); err != nil {
		t.Fatalf("InsertBenchmarkSessionMembers: %v", err)
	}
	if err := s.InsertBenchmarkScores(ctx, []benchmark.ScoreRecord{
		{
			AttemptID: aidA, RunID: runID, Scorer: "llm_judge", Score: 1, Passed: true,
			Rationale: "Test now passes deterministically across 20 runs.", JudgeModel: "sonnet-5",
		},
		{
			AttemptID: aidB, RunID: runID, Scorer: "llm_judge", Score: 0, Passed: false,
			Rationale: "Diff does not touch the flaky test file.", JudgeModel: "sonnet-5",
		},
	}); err != nil {
		t.Fatalf("InsertBenchmarkScores: %v", err)
	}
}

// TestSelectBenchmarkOrgRows_RunAndAttemptRows proves both row types are
// built correctly: per-config RunRows (grouping the two configs of one run
// into two rows sharing RunID), and per-terminal-attempt AttemptRows
// carrying the RAW fields (prompt/rationale/excerpt) LoadBenchmarkFacts
// itself does not select, merged with its derived cost/token facts.
func TestSelectBenchmarkOrgRows_RunAndAttemptRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	runID := "run-org-1"
	seedBenchmarkOrgRun(t, s, runID, time.Now().UTC().Add(-time.Hour))

	runRows, attemptRows, err := s.SelectBenchmarkOrgRows(ctx)
	if err != nil {
		t.Fatalf("SelectBenchmarkOrgRows: %v", err)
	}

	// -- Run rows: one per config, both present, both self-consistent.
	byConfig := map[string]int{}
	for _, r := range runRows {
		if r.RunID != runID {
			continue
		}
		byConfig[r.ConfigID]++
		if r.RunKey != runID+":"+r.ConfigID {
			t.Errorf("RunKey = %q, want %s:%s", r.RunKey, runID, r.ConfigID)
		}
		if r.TaskCount != 1 {
			t.Errorf("TaskCount = %d, want 1", r.TaskCount)
		}
		var repos []string
		if err := json.Unmarshal([]byte(r.RepoPathsJSON), &repos); err != nil {
			t.Fatalf("RepoPathsJSON did not decode: %v (%q)", err, r.RepoPathsJSON)
		}
		if len(repos) != 1 || repos[0] != "github.com/marmutapp/superbased-observer" {
			t.Errorf("RepoPathsJSON = %v, want the one seeded repo", repos)
		}
		if r.ExecutedCells != 1 {
			t.Errorf("config %s ExecutedCells = %d, want 1", r.ConfigID, r.ExecutedCells)
		}
		if r.ConfigHash == "" {
			t.Errorf("ConfigHash empty for config %s", r.ConfigID)
		}
		if r.Status != "completed" {
			t.Errorf("Status = %q, want completed", r.Status)
		}
		if r.StartedAt == "" || r.FinishedAt == "" {
			t.Errorf("StartedAt/FinishedAt should not be empty: %+v", r)
		}
	}
	if byConfig["codex-gpt5"] != 1 || byConfig["claude-sonnet"] != 1 {
		t.Fatalf("expected exactly one row per config, got %v", byConfig)
	}

	// codex-gpt5's attempt passed; claude-sonnet's did not.
	for _, r := range runRows {
		if r.RunID != runID {
			continue
		}
		switch r.ConfigID {
		case "codex-gpt5":
			if r.PassedCells != 1 {
				t.Errorf("codex-gpt5 PassedCells = %d, want 1", r.PassedCells)
			}
			if r.SpendUSD <= 0 {
				t.Errorf("codex-gpt5 SpendUSD should be > 0, got %v", r.SpendUSD)
			}
		case "claude-sonnet":
			if r.PassedCells != 0 {
				t.Errorf("claude-sonnet PassedCells = %d, want 0", r.PassedCells)
			}
		}
	}

	// -- Attempt rows: one per terminal attempt, carrying the raw fields.
	var gotA, gotB bool
	for _, a := range attemptRows {
		if a.RunID != runID {
			continue
		}
		if a.TaskPrompt != "Fix the flaky test in internal/store." {
			t.Errorf("TaskPrompt = %q", a.TaskPrompt)
		}
		if a.AttemptKey == "" || a.RunKey != runID+":"+a.ConfigID {
			t.Errorf("bad keys: %+v", a)
		}
		switch a.ConfigID {
		case "codex-gpt5":
			gotA = true
			if !a.Passed || !a.Scored {
				t.Errorf("codex-gpt5 attempt should be scored+passed: %+v", a)
			}
			if a.JudgeRationale != "Test now passes deterministically across 20 runs." {
				t.Errorf("JudgeRationale = %q", a.JudgeRationale)
			}
			if a.FinalAnswerExcerpt != "Patched the flaky assertion." {
				t.Errorf("FinalAnswerExcerpt = %q", a.FinalAnswerExcerpt)
			}
			if a.SpendUSD <= 0 || a.InputTokens <= 0 {
				t.Errorf("expected positive derived cost/tokens: %+v", a)
			}
			if !a.HasExitCode || a.ExitCode != 0 {
				t.Errorf("expected exit code 0: %+v", a)
			}
			if a.StartedAt == "" || a.FinishedAt == "" {
				t.Errorf("expected raw timing to be populated: %+v", a)
			}
		case "claude-sonnet":
			gotB = true
			if a.Passed {
				t.Errorf("claude-sonnet attempt should not be passed: %+v", a)
			}
			if a.JudgeRationale != "Diff does not touch the flaky test file." {
				t.Errorf("JudgeRationale = %q", a.JudgeRationale)
			}
		}
	}
	if !gotA || !gotB {
		t.Fatalf("expected one attempt row per config, gotA=%v gotB=%v", gotA, gotB)
	}
}

// TestSelectBenchmarkOrgRows_WindowExcludesOldRuns proves the trailing-window
// recompute (mirroring verbositySummaryWindowDays/sessionProcessWindowDays)
// excludes runs started well before the window.
func TestSelectBenchmarkOrgRows_WindowExcludesOldRuns(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	old := time.Now().UTC().AddDate(0, 0, -(benchmarkOrgRowsWindowDays + 10))
	seedBenchmarkOrgRun(t, s, "run-org-old", old)

	runRows, attemptRows, err := s.SelectBenchmarkOrgRows(ctx)
	if err != nil {
		t.Fatalf("SelectBenchmarkOrgRows: %v", err)
	}
	for _, r := range runRows {
		if r.RunID == "run-org-old" {
			t.Fatalf("expected run-org-old to be excluded by the trailing window, got a run row: %+v", r)
		}
	}
	for _, a := range attemptRows {
		if a.RunID == "run-org-old" {
			t.Fatalf("expected run-org-old to be excluded by the trailing window, got an attempt row: %+v", a)
		}
	}
}

// TestSelectBenchmarkOrgRows_AttemptCapKeepsMostRecentRuns proves the
// attempt-row cap (benchmarkAttemptRunCap) is applied at the RUN boundary,
// most-recent-first: runs beyond the cap still get a BenchmarkRunRow (so
// config identity + the leaderboard stay complete) but no attempt rows.
func TestSelectBenchmarkOrgRows_AttemptCapKeepsMostRecentRuns(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	var runIDs []string
	for i := 0; i < benchmarkAttemptRunCap+3; i++ {
		runID := fmt.Sprintf("run-org-cap-%02d", i)
		runIDs = append(runIDs, runID)
		// Space runs out so ORDER BY started_at DESC is unambiguous; the last
		// seeded run is the OLDEST here (i=0 -> earliest start).
		seedBenchmarkOrgRun(t, s, runID, base.Add(time.Duration(i)*time.Minute))
	}
	oldestRunID := runIDs[0]
	newestRunID := runIDs[len(runIDs)-1]

	runRows, attemptRows, err := s.SelectBenchmarkOrgRows(ctx)
	if err != nil {
		t.Fatalf("SelectBenchmarkOrgRows: %v", err)
	}

	seenRuns := map[string]bool{}
	for _, r := range runRows {
		seenRuns[r.RunID] = true
	}
	for _, id := range runIDs {
		if !seenRuns[id] {
			t.Errorf("expected a BenchmarkRunRow for every windowed run, missing %s", id)
		}
	}

	attemptRunIDs := map[string]bool{}
	for _, a := range attemptRows {
		attemptRunIDs[a.RunID] = true
	}
	if !attemptRunIDs[newestRunID] {
		t.Errorf("expected attempt rows for the most recent run %s", newestRunID)
	}
	if attemptRunIDs[oldestRunID] {
		t.Errorf("did not expect attempt rows for the oldest run %s beyond the cap", oldestRunID)
	}
}
