package admission

import (
	"strings"
	"testing"
)

// judgedFixture is a small judged criteria set used across the judge tests.
func judgedFixture() []Criterion {
	return []Criterion{
		{ID: "AD-100", Type: TypeValidUseCase, Name: "On-scope only", Definition: "Only product support questions.", Decision: DecisionDeny, Severity: SeverityWarn},
		{ID: "AD-400", Type: TypeCustom, Name: "No legal advice", Definition: "Do not answer legal-advice requests.", Decision: DecisionFlag, Severity: SeverityInfo},
	}
}

func TestBuildJudgePrompt_PerCriterionSectionsAndHints(t *testing.T) {
	judged := judgedFixture()
	p := buildJudgePrompt("how do I reset my password", judged)

	// Every criterion id appears as its own section.
	for _, c := range judged {
		if !strings.Contains(p, "id: "+c.ID) {
			t.Errorf("prompt missing section for criterion %q\n%s", c.ID, p)
		}
	}
	// Each criterion carries an explicit decision hint = its configured decision.
	if !strings.Contains(p, `if_violated_respond: "deny"`) {
		t.Errorf("prompt missing deny decision hint for AD-100\n%s", p)
	}
	if !strings.Contains(p, `if_violated_respond: "flag"`) {
		t.Errorf("prompt missing flag decision hint for AD-400\n%s", p)
	}
	// The response format forces one verdict per criterion echoing the id.
	if !strings.Contains(p, `"verdicts"`) || !strings.Contains(p, `"criterion"`) {
		t.Errorf("prompt missing per-criterion verdict response format\n%s", p)
	}
	// The request is embedded.
	if !strings.Contains(p, "how do I reset my password") {
		t.Errorf("prompt missing the request text\n%s", p)
	}
}

func TestParseJudgeVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantOK   bool
		wantLen  int
		wantHead judgeVerdict // first verdict (when wantLen>0)
	}{
		{
			name:     "wrapper object well-formed",
			raw:      `{"verdicts":[{"criterion":"AD-100","decision":"deny","reason":"off scope"},{"criterion":"AD-400","decision":"allow","reason":""}]}`,
			wantOK:   true,
			wantLen:  2,
			wantHead: judgeVerdict{Criterion: "AD-100", Decision: "deny", Reason: "off scope"},
		},
		{
			name:     "wrapper wrapped in code fence + prose",
			raw:      "Here is my answer:\n```json\n{\"verdicts\":[{\"criterion\":\"AD-400\",\"decision\":\"flag\",\"reason\":\"legal\"}]}\n```\nDone.",
			wantOK:   true,
			wantLen:  1,
			wantHead: judgeVerdict{Criterion: "AD-400", Decision: "flag", Reason: "legal"},
		},
		{
			name:     "bare array",
			raw:      `[{"criterion":"AD-100","decision":"ask","reason":"needs review"}]`,
			wantOK:   true,
			wantLen:  1,
			wantHead: judgeVerdict{Criterion: "AD-100", Decision: "ask", Reason: "needs review"},
		},
		{
			name:     "legacy single object (backward compat)",
			raw:      `{"decision":"deny","criterion":"AD-100","reason":"off scope"}`,
			wantOK:   true,
			wantLen:  1,
			wantHead: judgeVerdict{Criterion: "AD-100", Decision: "deny", Reason: "off scope"},
		},
		{
			name:    "legacy single allow object",
			raw:     `{"decision":"allow","reason":"ok"}`,
			wantOK:  true,
			wantLen: 1,
		},
		{
			name:   "prose only — no JSON",
			raw:    "I cannot comply with this request.",
			wantOK: false,
		},
		{
			name:   "empty reply",
			raw:    "",
			wantOK: false,
		},
		{
			name:    "sloppy trailing prose after object",
			raw:     `{"verdicts":[{"criterion":"AD-100","decision":"deny","reason":"nope"}]} -- hope that helps!`,
			wantOK:  true,
			wantLen: 1,
		},
		{
			name:    "empty verdicts array falls through to false",
			raw:     `{"verdicts":[]}`,
			wantOK:  false,
			wantLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseJudgeVerdicts(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if !ok {
				return
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d (%+v)", len(got), tt.wantLen, got)
			}
			if tt.wantHead != (judgeVerdict{}) && got[0] != tt.wantHead {
				t.Errorf("head = %+v, want %+v", got[0], tt.wantHead)
			}
		})
	}
}

func TestReduceVerdicts(t *testing.T) {
	judged := judgedFixture()

	tests := []struct {
		name     string
		verdicts []judgeVerdict
		wantOK   bool
		wantDec  Decision
		wantCrit string
		wantSev  Severity // severity comes from the attributed criterion
	}{
		{
			name:     "all allow → clean allow",
			verdicts: []judgeVerdict{{Criterion: "AD-100", Decision: "allow"}, {Criterion: "AD-400", Decision: "allow"}},
			wantOK:   true,
			wantDec:  DecisionAllow,
		},
		{
			name:     "strictest wins (deny over flag)",
			verdicts: []judgeVerdict{{Criterion: "AD-400", Decision: "flag", Reason: "legal"}, {Criterion: "AD-100", Decision: "deny", Reason: "off scope"}},
			wantOK:   true,
			wantDec:  DecisionDeny,
			wantCrit: "AD-100",
			wantSev:  SeverityWarn,
		},
		{
			name:     "attributes to the echoed criterion severity",
			verdicts: []judgeVerdict{{Criterion: "AD-400", Decision: "flag", Reason: "legal"}},
			wantOK:   true,
			wantDec:  DecisionFlag,
			wantCrit: "AD-400",
			wantSev:  SeverityInfo,
		},
		{
			name:     "unknown echoed id falls back to strictest judged criterion",
			verdicts: []judgeVerdict{{Criterion: "does-not-exist", Decision: "deny", Reason: "bad"}},
			wantOK:   true,
			wantDec:  DecisionDeny,
			wantCrit: "AD-100", // AD-100 is the strictest judged (deny > flag)
			wantSev:  SeverityWarn,
		},
		{
			name:     "all decisions unparseable → ok=false",
			verdicts: []judgeVerdict{{Criterion: "AD-100", Decision: "maybe"}},
			wantOK:   false,
		},
		{
			name:     "unparseable entry ignored, valid one wins",
			verdicts: []judgeVerdict{{Criterion: "AD-100", Decision: "maybe"}, {Criterion: "AD-400", Decision: "flag", Reason: "legal"}},
			wantOK:   true,
			wantDec:  DecisionFlag,
			wantCrit: "AD-400",
			wantSev:  SeverityInfo,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, ok := reduceVerdicts(tt.verdicts, judged)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if res.Decision != tt.wantDec {
				t.Errorf("decision = %v, want %v", res.Decision, tt.wantDec)
			}
			if tt.wantCrit != "" && res.Criterion != tt.wantCrit {
				t.Errorf("criterion = %q, want %q", res.Criterion, tt.wantCrit)
			}
			if tt.wantDec != DecisionAllow && res.Severity != tt.wantSev {
				t.Errorf("severity = %v, want %v", res.Severity, tt.wantSev)
			}
			if !res.JudgeUsed {
				t.Error("JudgeUsed = false, want true")
			}
			if tt.wantDec != DecisionAllow && res.Reason == "" {
				t.Error("non-allow verdict has empty reason")
			}
		})
	}
}
