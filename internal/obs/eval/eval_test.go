package eval

import (
	"context"
	"errors"
	"testing"
)

func TestCodeScorers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		spec         Spec
		sample       Sample
		wantPass     bool
		wantScore    float64
		wantBuildErr bool
	}{
		{name: "exact_match pass", spec: Spec{Name: "exact_match"}, sample: Sample{Output: "hi", Reference: "hi"}, wantPass: true, wantScore: 1},
		{name: "exact_match fail", spec: Spec{Name: "exact_match"}, sample: Sample{Output: "hi", Reference: "bye"}, wantPass: false, wantScore: 0},
		{name: "contains value param", spec: Spec{Name: "contains", Params: map[string]string{"value": "lo"}}, sample: Sample{Output: "hello"}, wantPass: true, wantScore: 1},
		{name: "contains reference", spec: Spec{Name: "contains"}, sample: Sample{Output: "hello", Reference: "ell"}, wantPass: true, wantScore: 1},
		{name: "icontains ci", spec: Spec{Name: "icontains", Params: map[string]string{"value": "HELLO"}}, sample: Sample{Output: "well hello there"}, wantPass: true, wantScore: 1},
		{name: "regex pass", spec: Spec{Name: "regex_match", Params: map[string]string{"pattern": `\d{3}`}}, sample: Sample{Output: "abc123"}, wantPass: true, wantScore: 1},
		{name: "regex missing param", spec: Spec{Name: "regex_match"}, wantBuildErr: true},
		{name: "json_valid pass", spec: Spec{Name: "json_valid"}, sample: Sample{Output: `{"a":1}`}, wantPass: true, wantScore: 1},
		{name: "json_valid fail", spec: Spec{Name: "json_valid"}, sample: Sample{Output: `{a:1`}, wantPass: false, wantScore: 0},
		{name: "non_empty fail", spec: Spec{Name: "non_empty"}, sample: Sample{Output: "   "}, wantPass: false, wantScore: 0},
		{name: "status_ok pass", spec: Spec{Name: "status_ok"}, sample: Sample{Facts: SpanFacts{Status: "ok"}}, wantPass: true, wantScore: 1},
		{name: "latency_under pass", spec: Spec{Name: "latency_under", Params: map[string]string{"ms": "2000"}}, sample: Sample{Facts: SpanFacts{DurationMS: 1500}}, wantPass: true, wantScore: 1},
		{name: "latency_under fail", spec: Spec{Name: "latency_under", Params: map[string]string{"ms": "1000"}}, sample: Sample{Facts: SpanFacts{DurationMS: 1500}}, wantPass: false, wantScore: 0},
		{name: "latency_under bad param", spec: Spec{Name: "latency_under", Params: map[string]string{"ms": "0"}}, wantBuildErr: true},
		{name: "cost_under pass", spec: Spec{Name: "cost_under", Params: map[string]string{"usd": "0.05"}}, sample: Sample{Facts: SpanFacts{CostUSD: 0.02}}, wantPass: true, wantScore: 1},
		{name: "cost_under fail", spec: Spec{Name: "cost_under", Params: map[string]string{"usd": "0.01"}}, sample: Sample{Facts: SpanFacts{CostUSD: 0.02}}, wantPass: false, wantScore: 0},
		{name: "unknown scorer", spec: Spec{Name: "nope"}, wantBuildErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := Build(tc.spec, nil)
			if tc.wantBuildErr {
				if err == nil {
					t.Fatalf("Build(%+v) = nil err, want error", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build(%+v): %v", tc.spec, err)
			}
			got, err := sc.Score(context.Background(), tc.sample)
			if err != nil {
				t.Fatalf("Score: %v", err)
			}
			if got.Passed != tc.wantPass || got.Score != tc.wantScore {
				t.Errorf("Score = {pass %v score %v}, want {pass %v score %v} (rationale %q)", got.Passed, got.Score, tc.wantPass, tc.wantScore, got.Rationale)
			}
		})
	}
}

// fakeJudge returns a canned reply (or an error) for llm_judge tests.
type fakeJudge struct {
	reply string
	err   error
}

func (f fakeJudge) Judge(_ context.Context, _ JudgeRequest) (JudgeResponse, error) {
	return JudgeResponse{Text: f.reply}, f.err
}

func TestLLMJudge(t *testing.T) {
	t.Parallel()

	if _, err := Build(Spec{Name: "llm_judge", Params: map[string]string{"prompt": "x"}}, nil); err == nil {
		t.Error("llm_judge with nil judge should fail to build")
	}
	if _, err := Build(Spec{Name: "llm_judge"}, fakeJudge{}); err == nil {
		t.Error("llm_judge with no prompt should fail to build")
	}

	sc, err := Build(Spec{Name: "llm_judge", Params: map[string]string{"prompt": "rate {{output}}", "threshold": "0.7"}}, fakeJudge{reply: `{"score":0.9,"rationale":"good"}`})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := sc.Score(context.Background(), Sample{Output: "answer"})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !got.Passed || got.Score != 0.9 || got.Rationale != "good" {
		t.Errorf("judge score = %+v, want pass 0.9 'good'", got)
	}

	// Below threshold → fail.
	sc2, _ := Build(Spec{Name: "llm_judge", Params: map[string]string{"prompt": "p", "threshold": "0.8"}}, fakeJudge{reply: "0.5"})
	got2, _ := sc2.Score(context.Background(), Sample{})
	if got2.Passed || got2.Score != 0.5 {
		t.Errorf("judge below threshold = %+v, want fail 0.5", got2)
	}

	// Judge error → scorer error (Run turns this into a closed-fail Result).
	sc3, _ := Build(Spec{Name: "llm_judge", Params: map[string]string{"prompt": "p"}}, fakeJudge{err: errors.New("boom")})
	if _, err := sc3.Score(context.Background(), Sample{}); err == nil {
		t.Error("judge error should surface as a scorer error")
	}
}

func TestRunAndSummarize(t *testing.T) {
	t.Parallel()
	ne, _ := Build(Spec{Name: "non_empty"}, nil)
	jv, _ := Build(Spec{Name: "json_valid"}, nil)
	samples := []Sample{
		{ItemID: 1, Output: `{"ok":true}`}, // non_empty pass, json_valid pass
		{ItemID: 2, Output: ``},            // both fail
	}
	results := Run(context.Background(), samples, []Scorer{ne, jv})
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	sum := Summarize(results)
	if sum.Total != 4 || sum.Passed != 2 {
		t.Errorf("summary = %+v, want total 4 passed 2", sum)
	}
	if sum.PassRate() != 0.5 {
		t.Errorf("pass rate = %v, want 0.5", sum.PassRate())
	}
}

// TestRunScorerErrorFailsClosed proves a scorer that errors becomes a visible
// Passed=false result rather than dropping the sample.
func TestRunScorerErrorFailsClosed(t *testing.T) {
	t.Parallel()
	sc, _ := Build(Spec{Name: "llm_judge", Params: map[string]string{"prompt": "p"}}, fakeJudge{err: errors.New("down")})
	results := Run(context.Background(), []Sample{{ItemID: 7}}, []Scorer{sc})
	if len(results) != 1 || results[0].Score.Passed {
		t.Fatalf("want 1 failed result, got %+v", results)
	}
	if results[0].Score.Scorer != "llm_judge" {
		t.Errorf("scorer name = %q, want llm_judge", results[0].Score.Scorer)
	}
}
