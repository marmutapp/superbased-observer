package guard

import (
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/policy"
)

// TestEvaluatePostureFindings routes dialect-drift findings through
// the REAL engine: each finding becomes one config_change verdict
// carrying R-204, category posture, and the finding detail in the
// reason; [guard.rules] disable silences the rule like any other.
func TestEvaluatePostureFindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	findings := []policy.PostureFinding{{
		Kind: policy.PostureFindingDialectDrift, Client: "claude-code",
		Target: "~/.claude/settings.json", Detail: "2 compiled entries missing",
	}}

	g := newTestGuard(t, mcpCfg("observe"), nil)
	verdicts := g.EvaluatePostureFindings(findings, now)
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(verdicts))
	}
	v := verdicts[0]
	if v.Verdict.RuleID != "R-204" || v.Category != "posture" || v.Verdict.Decision != policy.DecisionFlag {
		t.Fatalf("verdict = %s/%s/%s, want R-204/posture/flag", v.Verdict.RuleID, v.Category, v.Verdict.Decision)
	}
	if v.Input.Tool != "claude-code" || v.Input.Target != "~/.claude/settings.json" {
		t.Errorf("input = %+v, want client+path carried for the audit row", v.Input)
	}
	if !strings.Contains(v.Verdict.Reason, "2 compiled entries missing") {
		t.Errorf("reason %q missing finding detail", v.Verdict.Reason)
	}

	// [guard.rules] disable applies — the finding evaluates through
	// the real engine, not a side channel.
	cfg := mcpCfg("observe")
	cfg.Rules.Disable = []string{"R-204"}
	g2 := newTestGuard(t, cfg, nil)
	if got := g2.EvaluatePostureFindings(findings, now); len(got) != 0 {
		t.Fatalf("disabled R-204 still produced %d verdict(s)", len(got))
	}
}

// TestConfigWatch_TwoConsumers pins the generalized watched-config-
// paths trigger: the MCP consumer fires on locate-table registry
// writes, the dialect consumer fires on compiled-target writes, and
// neither fires on the other's paths, reads, or unrelated writes.
func TestConfigWatch_TwoConsumers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	in := func(actionType, target string) []ActionInput {
		return []ActionInput{{
			ActionID: 1, SessionID: "s1", ProjectRoot: "/home/u/proj",
			Tool: "claude-code", ActionType: actionType, Target: target, Timestamp: now,
		}}
	}
	cases := []struct {
		name        string
		inputs      []ActionInput
		wantMCP     bool
		wantDialect bool
	}{
		{"mcp registry write fires mcp only", in(models.ActionWriteFile, "/home/u/.claude.json"), true, false},
		{"dialect target write fires dialect only", in(models.ActionWriteFile, "/home/u/.claude/settings.json"), false, true},
		{"tilde-form dialect target", in(models.ActionEditFile, "~/.claude/settings.json"), false, true},
		{"read fires neither", in(models.ActionReadFile, "/home/u/.claude/settings.json"), false, false},
		{"unrelated write fires neither", in(models.ActionWriteFile, "/home/u/proj/x.json"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newTestGuard(t, mcpCfg("observe"), nil)
			mcpFired, dialectFired := false, false
			g.SetMCPRescan(func() { mcpFired = true })
			g.SetDialectRescan([]string{"/home/u/.claude/settings.json"}, func() { dialectFired = true })
			g.EvaluateActions(tc.inputs)
			if mcpFired != tc.wantMCP || dialectFired != tc.wantDialect {
				t.Errorf("fired mcp=%v dialect=%v, want mcp=%v dialect=%v",
					mcpFired, dialectFired, tc.wantMCP, tc.wantDialect)
			}
		})
	}
}
