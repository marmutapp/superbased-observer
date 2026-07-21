package main

import (
	"context"
	"os"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
	"github.com/marmutapp/superbased-observer/internal/obs/eval"
)

// evalScoreForTest builds a real eval-registry-backed evalScoreFn (a _test.go
// file is exempt from the obs reverse-import boundary), so the scorer tests
// exercise the actual reused registry rather than a hand-rolled fake.
func evalScoreForTest(judge eval.JudgeClient) evalScoreFn {
	return func(ctx context.Context, scorer string, params map[string]string, input, output, reference string) (float64, bool, string, bool, error) {
		sc, err := eval.Build(eval.Spec{Name: scorer, Params: params}, judge)
		if err != nil {
			return 0, false, "", false, nil
		}
		s, err := sc.Score(ctx, eval.Sample{Input: input, Output: output, Reference: reference})
		if err != nil {
			return 0, false, "", true, err
		}
		return s.Score, s.Passed, s.Rationale, true, nil
	}
}

func scoreInputFor(scorer, value, cmd, workspace, answer string) scoreInput {
	return scoreInput{
		Task: benchmark.Task{
			ID: "t", Prompt: "p",
			Success: benchmark.Success{Scorer: scorer, Value: value, Cmd: cmd},
		},
		AttemptID: 1, RunID: "r", WorkspaceDir: workspace, FinalAnswer: answer,
		Status: benchmark.StatusOK,
	}
}

func TestScoreTestsPass(t *testing.T) {
	t.Parallel()
	s := benchmarkScorer{}
	ws := t.TempDir()

	pass, err := s.Score(context.Background(), scoreInputFor("tests_pass", "", "true", ws, ""))
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(pass) != 1 || !pass[0].Passed {
		t.Errorf("expected pass, got %+v", pass)
	}

	fail, err := s.Score(context.Background(), scoreInputFor("tests_pass", "", "exit 3", ws, ""))
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(fail) != 1 || fail[0].Passed {
		t.Errorf("expected fail, got %+v", fail)
	}
	if fail[0].Rationale == "" {
		t.Error("failing test should carry a rationale")
	}
}

func TestScoreContainsViaRegistry(t *testing.T) {
	t.Parallel()
	s := benchmarkScorer{evalScore: evalScoreForTest(nil)}
	// Answer contains "middleware" → pass.
	got, err := s.Score(context.Background(), scoreInputFor("contains", "middleware", "", "", "the middleware chain runs"))
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(got) != 1 || !got[0].Passed || got[0].Scorer != "contains" {
		t.Errorf("contains pass: %+v", got)
	}
	// Missing substring → fail.
	miss, _ := s.Score(context.Background(), scoreInputFor("contains", "zzz", "", "", "no match here"))
	if len(miss) != 1 || miss[0].Passed {
		t.Errorf("contains fail: %+v", miss)
	}
}

func TestScoreEmptyAnswerIsUnavailable(t *testing.T) {
	t.Parallel()
	s := benchmarkScorer{evalScore: evalScoreForTest(nil)}
	// A text scorer with no recoverable final answer → no score (⇒ the runner
	// marks scorer_unavailable). Never a silent pass.
	got, err := s.Score(context.Background(), scoreInputFor("contains", "x", "", "", ""))
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty answer should yield no score, got %+v", got)
	}
}

func TestScoreLLMJudgeUnavailableWithoutJudge(t *testing.T) {
	t.Parallel()
	s := benchmarkScorer{evalScore: evalScoreForTest(nil)}
	in := scoreInput{
		Task: benchmark.Task{
			ID: "t", Prompt: "p",
			Success: benchmark.Success{Scorer: "llm_judge", Rubric: "names the middleware chain"},
		},
		AttemptID: 1, RunID: "r", FinalAnswer: "some answer",
	}
	got, err := s.Score(context.Background(), in)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("llm_judge without a judge should be unavailable, got %+v", got)
	}
}

// TestScoreContainsAll pins #1's echo-proof scorer: contains_all passes only
// when every declared token is present, and an empty answer is unavailable.
func TestScoreContainsAll(t *testing.T) {
	t.Parallel()
	s := benchmarkScorer{}
	mk := func(answer string, values ...string) scoreInput {
		return scoreInput{
			Task: benchmark.Task{
				ID: "t", Prompt: "p",
				Success: benchmark.Success{Scorer: "contains_all", Values: values},
			},
			AttemptID: 1, RunID: "r", FinalAnswer: answer, Status: benchmark.StatusOK,
		}
	}
	// All tokens present (case-insensitive) → pass.
	got, err := s.Score(context.Background(), mk("uses LIB/ROUTER and a Layer", "lib/router", "layer"))
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(got) != 1 || !got[0].Passed {
		t.Errorf("all-present should pass: %+v", got)
	}
	// One token missing → fail (with a rationale naming it).
	miss, _ := s.Score(context.Background(), mk("uses lib/router only", "lib/router", "layer"))
	if len(miss) != 1 || miss[0].Passed {
		t.Errorf("missing token should fail: %+v", miss)
	}
	// Empty answer → unavailable (no silent pass).
	empty, _ := s.Score(context.Background(), mk("", "lib/router"))
	if len(empty) != 0 {
		t.Errorf("empty answer should be unavailable, got %+v", empty)
	}
}

// TestWorkedCorpusScorersEchoProof is the headline #1 guarantee: EACH shipped
// task's scorer FAILS on a bare echo of its own prompt and PASSES on a
// plausible correct answer. A green verdict can no longer come from a prompt
// echo.
func TestWorkedCorpusScorersEchoProof(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../testdata/benchmark/coding-corpus-v1.toml")
	if err != nil {
		t.Skipf("worked corpus not present: %v", err)
	}
	spec, err := benchmark.ParseSpec(string(data))
	if err != nil {
		t.Fatalf("worked corpus parse: %v", err)
	}
	// A plausible correct answer per shipped task (contains the discriminating
	// tokens the prompt does NOT).
	correct := map[string]string{
		"express-lifecycle-summary": "The request enters at app.handle in lib/application.js:158, then " +
			"lib/router/index.js walks the middleware chain, dispatching each Layer (lib/router/layer.js:86) via next().",
		"lodash-chunk-doc": "_.chunk splits an array into groups of a given size (the last group holds the " +
			"remainder). It is defined in chunk.js at the lodash repo root.",
	}
	s := benchmarkScorer{evalScore: evalScoreForTest(nil)}
	for _, task := range spec.Tasks {
		// Bare prompt echo must NOT pass.
		echo, err := s.Score(context.Background(), scoreInput{
			Task: task, AttemptID: 1, RunID: "r", FinalAnswer: task.Prompt, Status: benchmark.StatusOK,
		})
		if err != nil {
			t.Fatalf("task %s echo score: %v", task.ID, err)
		}
		for _, rec := range echo {
			if rec.Passed {
				t.Errorf("task %s: a bare PROMPT ECHO passed the scorer — not echo-proof", task.ID)
			}
		}
		// A plausible correct answer MUST pass.
		ans, ok := correct[task.ID]
		if !ok {
			t.Fatalf("no correct-answer fixture for shipped task %q — add one", task.ID)
		}
		good, err := s.Score(context.Background(), scoreInput{
			Task: task, AttemptID: 1, RunID: "r", FinalAnswer: ans, Status: benchmark.StatusOK,
		})
		if err != nil {
			t.Fatalf("task %s correct score: %v", task.ID, err)
		}
		if len(good) != 1 || !good[0].Passed {
			t.Errorf("task %s: a plausible correct answer did NOT pass: %+v", task.ID, good)
		}
	}
}

// stubJudge returns a fixed structured reply for the judge path.
type stubJudge struct{ score float64 }

func (j stubJudge) Judge(_ context.Context, _ eval.JudgeRequest) (eval.JudgeResponse, error) {
	return eval.JudgeResponse{Text: `{"score": ` + f2s(j.score) + `, "rationale": "ok"}`}, nil
}

func f2s(f float64) string {
	if f == 1 {
		return "1"
	}
	return "0"
}

func TestScoreLLMJudgeWithStub(t *testing.T) {
	t.Parallel()
	s := benchmarkScorer{evalScore: evalScoreForTest(stubJudge{score: 1}), judgeModel: "test-judge"}
	in := scoreInput{
		Task: benchmark.Task{
			ID: "t", Prompt: "p",
			Success: benchmark.Success{Scorer: "llm_judge", Rubric: "names the middleware chain", Threshold: 0.5},
		},
		AttemptID: 1, RunID: "r", FinalAnswer: "the middleware chain is app.handle",
	}
	got, err := s.Score(context.Background(), in)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(got) != 1 || !got[0].Passed {
		t.Fatalf("judge pass expected, got %+v", got)
	}
	if got[0].JudgeModel != "test-judge" || got[0].RubricHash == "" {
		t.Errorf("judge provenance missing: %+v", got[0])
	}
}
