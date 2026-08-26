package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
)

func arenaFixtureRun(id string) *models.ArenaRun {
	return &models.ArenaRun{
		ID:          id,
		ProjectRoot: "/proj",
		BaseBranch:  "main",
		BaseSHA:     "abc123",
		Prompt:      "add a rate limiter",
		JudgeTool:   "claude-code",
		JudgeModel:  "judge-1",
		Status:      models.ArenaRunStatusPending,
	}
}

func TestArenaProcessBindingIsSyntheticAndScoped(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.BindArenaProcess(ctx, 4242, "ordinary-session", "aider", "/repo"); err == nil {
		t.Fatal("ordinary session id accepted as an Arena bridge")
	}
	sid := models.ArenaSessionIDPrefix + "run-aider"
	if err := s.BindArenaProcess(ctx, 4242, sid, "aider", "/repo"); err != nil {
		t.Fatal(err)
	}
	bridges := pidbridge.New(s.db)
	entry, ok, err := bridges.Lookup(ctx, 4242)
	if err != nil || !ok || entry.SessionID != sid || entry.Tool != "aider" {
		t.Fatalf("bound bridge = %+v ok=%v err=%v", entry, ok, err)
	}
	if err := bridges.Write(ctx, pidbridge.Entry{PID: 4242, SessionID: "new-owner", Tool: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UnbindArenaProcess(ctx, 4242, sid); err != nil {
		t.Fatal(err)
	}
	entry, ok, err = bridges.Lookup(ctx, 4242)
	if err != nil || !ok || entry.SessionID != "new-owner" {
		t.Fatalf("scoped unbind removed a newer PID owner: %+v ok=%v err=%v", entry, ok, err)
	}
}

func TestArenaRunRoundtrip(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ArenaRun(ctx, "missing"); err != nil {
		t.Fatalf("missing run lookup: %v", err)
	}

	run := arenaFixtureRun("run-1")
	run.CreatedAt = ""
	run.UpdatedAt = ""
	if err := s.InsertArenaRun(ctx, run); err != nil {
		t.Fatalf("InsertArenaRun: %v", err)
	}
	if run.CreatedAt == "" || run.UpdatedAt == "" {
		t.Fatal("store must stamp created_at/updated_at")
	}

	got, err := s.ArenaRun(ctx, "run-1")
	if err != nil || got == nil {
		t.Fatalf("ArenaRun: %v %v", got, err)
	}
	if got.ProjectRoot != "/proj" || got.BaseSHA != "abc123" || got.Status != models.ArenaRunStatusPending {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	if err := s.UpdateArenaRunStatus(ctx, "run-1", models.ArenaRunStatusRunning); err != nil {
		t.Fatalf("UpdateArenaRunStatus: %v", err)
	}
	got, _ = s.ArenaRun(ctx, "run-1")
	if got.Status != models.ArenaRunStatusRunning || got.UpdatedAt == run.UpdatedAt {
		t.Fatalf("status transition not persisted: %+v", got)
	}

	recent, err := s.RecentArenaRuns(ctx, 10)
	if err != nil || len(recent) != 1 {
		t.Fatalf("RecentArenaRuns len=%d err=%v", len(recent), err)
	}
}

func TestArenaCandidateRoundtrip(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertArenaRun(ctx, arenaFixtureRun("run-c")); err != nil {
		t.Fatal(err)
	}

	cand := &models.ArenaCandidate{
		ID:                 "cand-1",
		RunID:              "run-c",
		Tool:               "codex",
		Model:              "m1",
		Seq:                0,
		Status:             models.ArenaCandidateStatusPending,
		BranchName:         "arena/run-c/codex",
		WorktreePath:       filepath.Join("~", "observer", "arena", "run-c", "codex"),
		SessionIDs:         []string{"ses-a", "ses-b"},
		Scores:             &models.JudgeScores{Correctness: 7, Completeness: 5, CodeQuality: 8, Performance: 6, Risk: 3, Overall: 7, VerdictRationale: "solid"},
		FinalAnswerExcerpt: "done",
		DiffFiles:          2,
		DiffAdded:          10,
		DiffRemoved:        4,
		InputTokens:        1200,
		OutputTokens:       340,
		CostUSD:            0.012,
		TimedOut:           true,
		ExitCode:           -1,
		WallMS:             45000,
	}
	if err := s.InsertArenaCandidate(ctx, cand); err != nil {
		t.Fatalf("InsertArenaCandidate: %v", err)
	}

	got, err := s.ArenaCandidates(ctx, "run-c")
	if err != nil || len(got) != 1 {
		t.Fatalf("ArenaCandidates: %d %v", len(got), err)
	}
	c := got[0]
	if !c.TimedOut || c.ExitCode != -1 || c.WallMS != 45000 ||
		len(c.SessionIDs) != 2 || c.SessionIDs[1] != "ses-b" ||
		c.Scores == nil || c.Scores.CodeQuality != 8 || c.Scores.Risk != 3 ||
		c.CostUSD < 0.0119 || c.InputTokens != 1200 {
		t.Fatalf("JSON/metric roundtrip mismatch: %+v", c)
	}
	one, err := s.ArenaCandidate(ctx, "cand-1")
	if err != nil || one == nil || one.ID != c.ID || one.Scores == nil || one.Scores.Overall != 7 {
		t.Fatalf("ArenaCandidate roundtrip = %+v err=%v", one, err)
	}
	if missing, err := s.ArenaCandidate(ctx, "missing"); err != nil || missing != nil {
		t.Fatalf("missing ArenaCandidate = %+v err=%v", missing, err)
	}

	// Full-row update through the judged lifecycle.
	c.Status = models.ArenaCandidateStatusJudged
	c.Scores.Overall = 9
	c.SessionIDs = []string{"ses-a"}
	if err := s.UpdateArenaCandidate(ctx, &c); err != nil {
		t.Fatalf("UpdateArenaCandidate: %v", err)
	}
	got, _ = s.ArenaCandidates(ctx, "run-c")
	if got[0].Status != models.ArenaCandidateStatusJudged ||
		got[0].Scores.Overall != 9 || len(got[0].SessionIDs) != 1 {
		t.Fatalf("update mismatch: %+v", got[0])
	}

	// Keep / discard transitions are compare-and-set lifecycle edges.
	if err := s.SetCandidateKept(ctx, "cand-1", "deadbeef"); err != nil {
		t.Fatalf("SetCandidateKept: %v", err)
	}
	got, _ = s.ArenaCandidates(ctx, "run-c")
	if got[0].Status != models.ArenaCandidateStatusKept || got[0].KeptCommitSHA != "deadbeef" {
		t.Fatalf("keep mismatch: %+v", got[0])
	}
	if err := s.SetCandidateDiscarded(ctx, "cand-1"); err == nil {
		t.Fatal("kept candidate was allowed to transition to discarded")
	}
	discard := *cand
	discard.ID = "cand-2"
	discard.Seq = 1
	discard.Status = models.ArenaCandidateStatusFailed
	discard.KeptCommitSHA = ""
	if err := s.InsertArenaCandidate(ctx, &discard); err != nil {
		t.Fatalf("Insert discard candidate: %v", err)
	}
	if err := s.SetCandidateDiscarded(ctx, "cand-2"); err != nil {
		t.Fatalf("SetCandidateDiscarded: %v", err)
	}
	got, _ = s.ArenaCandidates(ctx, "run-c")
	if got[1].Status != models.ArenaCandidateStatusDiscarded {
		t.Fatalf("discard mismatch: %+v", got[1])
	}
}

func TestArenaCandidateOrderingBySeq(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertArenaRun(ctx, arenaFixtureRun("run-o")); err != nil {
		t.Fatal(err)
	}
	for i, tool := range []string{"claude-code", "codex"} {
		if err := s.InsertArenaCandidate(ctx, &models.ArenaCandidate{
			ID: tool, RunID: "run-o", Tool: tool, Seq: i,
			Status:     models.ArenaCandidateStatusPending,
			SessionIDs: []string{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ArenaCandidates(ctx, "run-o")
	if err != nil || len(got) != 2 {
		t.Fatalf("candidates: %d %v", len(got), err)
	}
	for i, want := range []string{"claude-code", "codex"} {
		if got[i].Tool != want {
			t.Fatalf("seq order broken: [%d]=%s want %s", i, got[i].Tool, want)
		}
	}
}

func TestArenaInsertValidation(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertArenaRun(ctx, &models.ArenaRun{ID: ""}); err == nil {
		t.Fatal("empty-id run accepted")
	}
	if err := s.InsertArenaCandidate(ctx, &models.ArenaCandidate{ID: "x"}); err == nil {
		t.Fatal("missing run_id/tool candidate accepted")
	}
}

func TestArenaUpdatesRejectMissingRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.UpdateArenaRunStatus(ctx, "missing", models.ArenaRunStatusFailed); err == nil {
		t.Fatal("missing run status update succeeded")
	}
	if err := s.UpdateArenaCandidate(ctx, &models.ArenaCandidate{ID: "missing"}); err == nil {
		t.Fatal("missing candidate update succeeded")
	}
	if err := s.SetCandidateKept(ctx, "missing", "deadbeef"); err == nil {
		t.Fatal("missing candidate keep succeeded")
	}
	if err := s.SetCandidateDiscarded(ctx, "missing"); err == nil {
		t.Fatal("missing candidate discard succeeded")
	}
}
