package export

import (
	"strings"
	"testing"
	"time"
)

var reportNow = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

// TestParsePeriod pins the three accepted forms and the rejects.
func TestParsePeriod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantStart time.Time
		wantEnd   time.Time
		wantErr   bool
	}{
		{"168h", reportNow.Add(-168 * time.Hour), reportNow, false},
		{"720h", reportNow.Add(-720 * time.Hour), reportNow, false},
		{"2026-05", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), false},
		{"2025-12", time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"2026-06-10", time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC), false},
		{" 168h ", reportNow.Add(-168 * time.Hour), reportNow, false}, // trimmed
		{"", time.Time{}, time.Time{}, true},
		{"-24h", time.Time{}, time.Time{}, true},
		{"0s", time.Time{}, time.Time{}, true},
		{"last-week", time.Time{}, time.Time{}, true},
		{"2026-13", time.Time{}, time.Time{}, true},
	}
	for _, c := range cases {
		p, err := ParsePeriod(c.in, reportNow)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsePeriod(%q): expected error, got %+v", c.in, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePeriod(%q): %v", c.in, err)
			continue
		}
		if !p.Start.Equal(c.wantStart) || !p.End.Equal(c.wantEnd) {
			t.Errorf("ParsePeriod(%q) = [%s, %s), want [%s, %s)",
				c.in, p.Start, p.End, c.wantStart, c.wantEnd)
		}
		if p.Label != strings.TrimSpace(c.in) {
			t.Errorf("ParsePeriod(%q): label = %q", c.in, p.Label)
		}
	}
}

// logEntry builds a policy-state load row.
func logEntry(layer, path, hash string, at time.Time) PolicyStateEntry {
	return PolicyStateEntry{Layer: layer, Path: path, ContentHash: hash, LoadedAt: at}
}

// TestBuildReportPolicySlicing pins the §14.4 policy-log time
// slicing: effective-at boundaries (latest load per (layer, path) at
// or before the instant) and the in-period change log.
func TestBuildReportPolicySlicing(t *testing.T) {
	t.Parallel()
	period := Period{Label: "test", Start: reportNow.Add(-48 * time.Hour), End: reportNow}
	log := []PolicyStateEntry{
		logEntry("user", "/u/policy.toml", "hash-v1", reportNow.Add(-100*time.Hour)), // before period
		logEntry("org", "https://org/api#policy-key", "key-1", reportNow.Add(-90*time.Hour)),
		logEntry("user", "/u/policy.toml", "hash-v2", reportNow.Add(-24*time.Hour)), // inside period
		logEntry("project", "/p/.observer/guard-policy.toml", "proj-1", reportNow.Add(-12*time.Hour)),
		logEntry("user", "/u/policy.toml", "hash-v3", reportNow.Add(time.Hour)), // after period
	}
	r := BuildReport(ReportInput{
		GeneratedAt: reportNow, Period: period, GuardEnabled: true, Mode: "observe",
		PolicyLog: log,
	})

	// At start: only the two pre-period loads, sorted layer then path.
	if len(r.PolicyAtStart) != 2 {
		t.Fatalf("PolicyAtStart: got %d entries, want 2: %+v", len(r.PolicyAtStart), r.PolicyAtStart)
	}
	if r.PolicyAtStart[0].Layer != "org" || r.PolicyAtStart[1].ContentHash != "hash-v1" {
		t.Errorf("PolicyAtStart wrong: %+v", r.PolicyAtStart)
	}

	// At end: user superseded by v2 (not v3 — that's after the period),
	// project now present.
	if len(r.PolicyAtEnd) != 3 {
		t.Fatalf("PolicyAtEnd: got %d entries, want 3: %+v", len(r.PolicyAtEnd), r.PolicyAtEnd)
	}
	for _, e := range r.PolicyAtEnd {
		if e.Layer == "user" && e.ContentHash != "hash-v2" {
			t.Errorf("user layer at end = %q, want hash-v2 (v3 is after the period)", e.ContentHash)
		}
	}

	// Changes: exactly the two in-period loads, in load order.
	if len(r.PolicyChanges) != 2 || r.PolicyChanges[0].ContentHash != "hash-v2" || r.PolicyChanges[1].ContentHash != "proj-1" {
		t.Errorf("PolicyChanges wrong: %+v", r.PolicyChanges)
	}
}

// TestBuildReportBoundaryInclusion pins the half-open window edges on
// the change log: a load exactly at Start is in, exactly at End is out.
func TestBuildReportBoundaryInclusion(t *testing.T) {
	t.Parallel()
	period := Period{Start: reportNow.Add(-time.Hour), End: reportNow}
	log := []PolicyStateEntry{
		logEntry("user", "/u", "at-start", period.Start),
		logEntry("user", "/u", "at-end", period.End),
	}
	r := BuildReport(ReportInput{Period: period, PolicyLog: log})
	if len(r.PolicyChanges) != 1 || r.PolicyChanges[0].ContentHash != "at-start" {
		t.Errorf("half-open window violated: %+v", r.PolicyChanges)
	}
	// The at-End load IS effective at End ("at or before").
	if len(r.PolicyAtEnd) != 1 || r.PolicyAtEnd[0].ContentHash != "at-end" {
		t.Errorf("effective-at-end must include a load at the boundary instant: %+v", r.PolicyAtEnd)
	}
}

// TestRenderText smoke-pins every §14.4 section header plus the
// empty-state placeholders, so a dropped section fails loudly.
func TestRenderText(t *testing.T) {
	t.Parallel()
	r := BuildReport(ReportInput{
		GeneratedAt:  reportNow,
		Period:       Period{Label: "720h", Start: reportNow.Add(-720 * time.Hour), End: reportNow},
		GuardEnabled: true, Mode: "enforce", ObserverVersion: "1.8.3",
		PolicyLog: []PolicyStateEntry{
			logEntry("user", "/u/policy.toml", "abcdef0123456789", reportNow.Add(-100*time.Hour)),
		},
		Stats: VerdictStats{
			Total: 12, Enforced: 3,
			ByDecision: map[string]int{"deny": 3, "flag": 9},
			BySeverity: map[string]int{"high": 5, "critical": 7},
			ByCategory: map[string]int{"destructive": 12},
		},
		Chain: ChainResult{Checked: 40, OK: true},
		Coverage: []CoverageRow{
			{Client: "claude-code", Channel: "hook:PreToolUse", PreExecution: true, CanBlock: true, CanAsk: true},
			{Client: "codex", Channel: "watcher"},
		},
		ActiveApprovals: []ApprovalEntry{
			{
				ID: 4, TS: reportNow.Add(-2 * time.Hour), RuleID: "R-110", Scope: "session",
				SessionID: "s1", GrantedBy: "llm_judge", ExpiresAt: reportNow.Add(time.Hour),
			},
		},
	})
	text := RenderText(r)
	for _, want := range []string{
		"Guard evidence pack — period 720h",
		"observer 1.8.3 · guard on mode enforce",
		"== Effective policy at period start ==",
		"== Effective policy at period end ==",
		"== Policy changes in period ==",
		"== Verdict statistics ==",
		"total 12, enforced 3",
		"by decision:",
		"deny=3 flag=9",
		"== Audit-chain verification ==",
		"OK — 40 row(s) verified",
		"== Coverage matrix (§6.5) ==",
		"pre-exec+block+ask",
		"observe-only",
		"== Exception register (active approvals) ==",
		"#4 R-110 scope=session granted_by=llm_judge",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("RenderText missing %q in:\n%s", want, text)
		}
	}

	// Broken chain renders the divergence, not OK.
	r.Chain = ChainResult{Checked: 40, OK: false, FirstDivergenceID: 17, Detail: "rewritten row at id 17"}
	text = RenderText(r)
	if !strings.Contains(text, "BROKEN at id 17") || !strings.Contains(text, "rewritten row at id 17") {
		t.Errorf("broken chain not rendered:\n%s", text)
	}

	// Empty report renders placeholders rather than vanishing sections.
	empty := RenderText(BuildReport(ReportInput{Period: Period{Label: "24h"}}))
	for _, want := range []string{"(no policy sources recorded)", "(none)"} {
		if !strings.Contains(empty, want) {
			t.Errorf("empty report missing placeholder %q:\n%s", want, empty)
		}
	}
}
