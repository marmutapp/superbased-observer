package processobs

import (
	"testing"
	"time"
)

func TestDeriveCommandRuns(t *testing.T) {
	t0 := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	ti := 3
	actions := []ActionRef{
		{ActionID: 10, TurnIndex: &ti, Command: "sed -n '1,5p' README.md", Timestamp: t0, Duration: 680 * time.Millisecond, Success: true},
		{ActionID: 11, Command: "rg --files", Timestamp: t0.Add(time.Second), Success: false},                    // failed, no duration
		{ActionID: 12, Command: "go build ./...", Timestamp: t0.Add(2 * time.Second), Duration: 9 * time.Second}, // OS-anchored
	}
	osLinked := map[int64]bool{12: true}

	runs := DeriveCommandRuns("sess-1", "codex", 7, actions, osLinked)
	if len(runs) != 2 {
		t.Fatalf("want 2 derived runs (action 12 is OS-anchored), got %d", len(runs))
	}

	byKey := map[string]ProcessRun{}
	for _, r := range runs {
		byKey[r.ProcessKey] = r
	}

	r10, ok := byKey["derived:action:10"]
	if !ok {
		t.Fatalf("missing derived row for action 10; keys=%v", keysOf(byKey))
	}
	if r10.PID != 0 {
		t.Errorf("derived row pid: want 0, got %d", r10.PID)
	}
	if r10.Attribution.Source != AttrActionCorrelation || r10.Attribution.Confidence != ConfHigh {
		t.Errorf("attribution: want action_correlation/high, got %s/%s", r10.Attribution.Source, r10.Attribution.Confidence)
	}
	if r10.Attribution.ActionID == nil || *r10.Attribution.ActionID != 10 {
		t.Errorf("action link not intrinsic: %v", r10.Attribution.ActionID)
	}
	if r10.Attribution.TurnIndex == nil || *r10.Attribution.TurnIndex != 3 {
		t.Errorf("turn index: want 3, got %v", r10.Attribution.TurnIndex)
	}
	if r10.Attribution.SessionID != "sess-1" || r10.Attribution.Tool != "codex" || r10.Attribution.ProjectID != 7 {
		t.Errorf("session meta wrong: %+v", r10.Attribution)
	}
	if r10.ExeBasename != "sed" {
		t.Errorf("exe basename: want sed, got %q", r10.ExeBasename)
	}
	if !r10.Exited || r10.ExitCode != 0 {
		t.Errorf("success run: want exited/exit0, got exited=%v code=%d", r10.Exited, r10.ExitCode)
	}
	if r10.DurationMs != 680 {
		t.Errorf("duration_ms: want 680, got %d", r10.DurationMs)
	}
	if !r10.ExitedAt.Equal(t0.Add(680 * time.Millisecond)) {
		t.Errorf("exited_at should be start+duration, got %v", r10.ExitedAt)
	}

	r11 := byKey["derived:action:11"]
	if !r11.Exited || r11.ExitCode != 1 {
		t.Errorf("failed run: want exited/exit1, got exited=%v code=%d", r11.Exited, r11.ExitCode)
	}
	if !r11.ExitedAt.Equal(r11.StartedAt) {
		t.Errorf("zero-duration run: exited_at should equal started_at, got %v vs %v", r11.ExitedAt, r11.StartedAt)
	}

	if _, found := byKey["derived:action:12"]; found {
		t.Errorf("action 12 is OS-anchored and must not be derived")
	}
}

func TestDeriveCommandRuns_Empty(t *testing.T) {
	if got := DeriveCommandRuns("", "codex", 0, nil, nil); got != nil {
		t.Errorf("empty session: want nil, got %v", got)
	}
	if got := DeriveCommandRuns("s", "codex", 0, nil, nil); got != nil {
		t.Errorf("no actions: want nil, got %v", got)
	}
	// Every action OS-anchored → nothing to synthesize.
	acts := []ActionRef{{ActionID: 1, Command: "ls"}}
	if got := DeriveCommandRuns("s", "codex", 0, acts, map[int64]bool{1: true}); got != nil {
		t.Errorf("all-anchored: want nil, got %v", got)
	}
}

func keysOf(m map[string]ProcessRun) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
