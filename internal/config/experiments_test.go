package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidateExperiment(t *testing.T) {
	t.Parallel()
	base := ExperimentConfig{Name: "a4-cline", Class: "tool:cline", Control: "claude-code", Candidate: "cand-cline"}
	cases := []struct {
		name    string
		mutate  func(*ExperimentConfig)
		wantErr string
	}{
		{"valid tool class", func(e *ExperimentConfig) {}, ""},
		{"valid provider class", func(e *ExperimentConfig) { e.Class = "openai" }, ""},
		{"bad name", func(e *ExperimentConfig) { e.Name = "Bad Name" }, "invalid experiment name"},
		{"bad class", func(e *ExperimentConfig) { e.Class = "everything" }, "invalid experiment class"},
		{"empty tool class", func(e *ExperimentConfig) { e.Class = "tool:" }, "invalid experiment class"},
		{"missing arm", func(e *ExperimentConfig) { e.Candidate = "" }, "needs both"},
		{"same arms", func(e *ExperimentConfig) { e.Candidate = e.Control }, "arms must differ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.mutate(&e)
			err := ValidateExperiment(e)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v want %q", err, tc.wantErr)
			}
		})
	}
}

func TestActiveExperimentFor(t *testing.T) {
	t.Parallel()
	exps := []ExperimentConfig{
		{Name: "done", Class: "openai", Control: "a", Candidate: "b", StartedAt: "2026-06-01T00:00:00Z", StoppedAt: "2026-06-02T00:00:00Z"},
		{Name: "oai", Class: "openai", Control: "a", Candidate: "b", StartedAt: "2026-06-10T00:00:00Z"},
		{Name: "cline", Class: "tool:cline", Control: "a", Candidate: "b", StartedAt: "2026-06-10T00:00:00Z"},
	}
	cases := []struct {
		name     string
		provider string
		tool     string
		want     string // experiment name, "" for nil
	}{
		{"provider match skips stopped", "openai", "", "oai"},
		{"provider match with unrelated tool", "openai", "codex", "oai"},
		{"tool match", "anthropic", "cline", "cline"},
		{"tool class needs resolved tool", "anthropic", "", ""},
		{"no match", "anthropic", "kilo-code", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ActiveExperimentFor(exps, tc.provider, tc.tool)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("want nil, got %q", got.Name)
			case tc.want != "" && (got == nil || got.Name != tc.want):
				t.Fatalf("want %q, got %+v", tc.want, got)
			}
		})
	}
}

func TestExperimentArm_DeterministicAndSalted(t *testing.T) {
	t.Parallel()
	e1 := ExperimentConfig{Name: "exp-one", Control: "ctl", Candidate: "cand"}
	e2 := ExperimentConfig{Name: "exp-two", Control: "ctl", Candidate: "cand"}

	p1, a1 := ExperimentArm(e1, "session-x")
	p2, a2 := ExperimentArm(e1, "session-x")
	if p1 != p2 || a1 != a2 {
		t.Fatalf("not deterministic: %s/%s vs %s/%s", p1, a1, p2, a2)
	}
	if (a1 == "control") != (p1 == "ctl") {
		t.Fatalf("arm/profile mismatch: %s/%s", p1, a1)
	}

	// Salting: across many sessions the two experiments must not agree
	// everywhere (independent splits), and both arms must occur.
	agree, e1Control := 0, 0
	const n = 200
	for i := 0; i < n; i++ {
		sid := "s-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + time.Time{}.Add(time.Duration(i)).String()
		_, x := ExperimentArm(e1, sid)
		_, y := ExperimentArm(e2, sid)
		if x == y {
			agree++
		}
		if x == "control" {
			e1Control++
		}
	}
	if agree == n {
		t.Error("two differently-named experiments split identically — salt not applied")
	}
	if e1Control == 0 || e1Control == n {
		t.Errorf("degenerate split: %d/%d control", e1Control, n)
	}
}

func TestResolveProfileNameForSession(t *testing.T) {
	t.Parallel()
	pc := ProfilesConfig{
		Default:    "default",
		ByProvider: map[string]string{"openai": "codex-safe"},
		ByTool:     map[string]string{"cline": "claude-code"},
	}
	running := []ExperimentConfig{
		{Name: "oai-exp", Class: "openai", Control: "codex-safe", Candidate: "cand-x", StartedAt: "2026-06-10T00:00:00Z"},
	}
	cases := []struct {
		name      string
		exps      []ExperimentConfig
		provider  string
		tool      string
		sessionID string
		wantExp   string
	}{
		{"experiment claims matching session", running, "openai", "", "s1", "oai-exp"},
		{"experiment beats by_tool for its class", running, "openai", "cline", "s1", "oai-exp"},
		{"empty session skips experiments", running, "openai", "", "", ""},
		{"non-matching provider falls through", running, "anthropic", "", "s1", ""},
		{"no experiments = plain table", nil, "openai", "", "s1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile, expName, arm := ResolveProfileNameForSession(pc, tc.exps, tc.provider, tc.tool, tc.sessionID)
			if expName != tc.wantExp {
				t.Fatalf("experiment: got %q want %q", expName, tc.wantExp)
			}
			if tc.wantExp == "" {
				if arm != "" {
					t.Fatalf("arm leaked without experiment: %q", arm)
				}
				if want := ResolveProfileName(pc, tc.provider, tc.tool); profile != want {
					t.Fatalf("plain resolution: got %q want %q", profile, want)
				}
				return
			}
			if arm != "control" && arm != "candidate" {
				t.Fatalf("arm: %q", arm)
			}
			wantProfile, wantArm := ExperimentArm(running[0], tc.sessionID)
			if profile != wantProfile || arm != wantArm {
				t.Fatalf("arm assignment: got %s/%s want %s/%s", profile, arm, wantProfile, wantArm)
			}
		})
	}
}

func TestExperimentWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	running := ExperimentConfig{Name: "r", StartedAt: "2026-06-10T00:00:00Z"}
	start, end, err := ExperimentWindow(running, now)
	if err != nil || !end.Equal(now) || start.Day() != 10 {
		t.Fatalf("running window: %v %v %v", start, end, err)
	}
	stopped := ExperimentConfig{Name: "s", StartedAt: "2026-06-09T00:00:00Z", StoppedAt: "2026-06-10T06:00:00Z"}
	_, end, err = ExperimentWindow(stopped, now)
	if err != nil || end.Hour() != 6 {
		t.Fatalf("stopped window: %v %v", end, err)
	}
	if _, _, err := ExperimentWindow(ExperimentConfig{Name: "bad", StartedAt: "nope"}, now); err == nil {
		t.Fatal("bad timestamp accepted")
	}
}
