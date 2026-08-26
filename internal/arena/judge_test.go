package arena

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestParseJudgeScores(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		answer  string
		wantErr bool
		check   func(*testing.T, *models.JudgeScores)
	}{
		{
			name:   "plain json",
			answer: `{"correctness":7,"completeness":8,"code_quality":6,"performance":5,"risk":2,"overall":7,"verdict_rationale":"ok"}`,
			check: func(t *testing.T, s *models.JudgeScores) {
				if s.Overall != 7 || s.Risk != 2 || s.VerdictRationale != "ok" {
					t.Fatalf("%+v", s)
				}
			},
		},
		{
			name:   "fenced json",
			answer: "```json\n{\"correctness\":1,\"completeness\":1,\"code_quality\":1,\"performance\":1,\"risk\":1,\"overall\":1,\"verdict_rationale\":\"\"}\n```",
			check: func(t *testing.T, s *models.JudgeScores) {
				if s.Overall != 1 {
					t.Fatalf("overall=%d", s.Overall)
				}
			},
		},
		{
			name:    "out of range",
			answer:  `{"correctness":11,"completeness":1,"code_quality":1,"performance":1,"risk":1,"overall":1}`,
			wantErr: true,
		},
		{
			name:    "missing field",
			answer:  `{"correctness":5,"completeness":1,"code_quality":1,"performance":1,"risk":1}`,
			wantErr: true,
		},
		{
			name:    "no json",
			answer:  "I think it is fine.",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseJudgeScores(tc.answer)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tc.check(t, s)
		})
	}
}

func TestRenderJudgePromptTruncation(t *testing.T) {
	big := strings.Repeat("x", MaxJudgePatchBytes+1000)
	p := renderJudgePrompt("task", big)
	if len(p) >= len(big)+len(renderJudgePrompt("task", "")) {
		t.Fatalf("prompt not truncated: %d", len(p))
	}
	if !strings.Contains(p, "[diff truncated") {
		t.Fatal("truncation marker missing")
	}
	small := renderJudgePrompt("task", "tiny diff")
	if !strings.Contains(small, "<patch>\ntiny diff\n</patch>") {
		t.Fatal("small patch not embedded verbatim")
	}
}
