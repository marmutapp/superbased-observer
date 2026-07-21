package termstatus

import (
	"testing"
	"time"
)

// base is a fixed "now" so age math is deterministic.
var base = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return base.Add(-d) }

// TestClassify is the accuracy harness (plan F4 "measure precision/recall on
// fixtures before marketing"): one fixture row per case, asserting the fused
// status + its confidence tier. It doubles as the regression guard for the
// ordered rule table.
func TestClassify(t *testing.T) {
	code0 := 0
	tests := []struct {
		name     string
		sig      Signals
		want     Status
		wantConf Confidence
	}{
		{
			name:     "no signal at all → unknown",
			sig:      Signals{Now: base},
			want:     StatusUnknown,
			wantConf: ConfNone,
		},
		{
			name:     "process exited is trusted and wins",
			sig:      Signals{Now: base, Exited: true, ExitCode: &code0, LastOutput: ago(time.Second)},
			want:     StatusExited,
			wantConf: ConfTrusted,
		},
		{
			name:     "trusted hook pre_tool → working, beats stale hints",
			sig:      Signals{Now: base, LastHookKind: HookPreTool, LastHookAt: ago(2 * time.Second), LastOutput: ago(10 * time.Minute)},
			want:     StatusWorking,
			wantConf: ConfTrusted,
		},
		{
			name:     "trusted notification hook → waiting",
			sig:      Signals{Now: base, LastHookKind: HookNotification, LastHookAt: ago(3 * time.Second)},
			want:     StatusWaitingForInput,
			wantConf: ConfTrusted,
		},
		{
			name:     "trusted blocked hook → blocked",
			sig:      Signals{Now: base, LastHookKind: HookBlocked, LastHookAt: ago(1 * time.Second)},
			want:     StatusBlocked,
			wantConf: ConfTrusted,
		},
		{
			name:     "stale hook is ignored (falls through to hints)",
			sig:      Signals{Now: base, LastHookKind: HookPreTool, LastHookAt: ago(10 * time.Minute), LastOutput: ago(time.Second)},
			want:     StatusWorking, // via output_recent hint, not the stale hook
			wantConf: ConfHint,
		},
		{
			name: "prompt mark + silence → waiting (the flagship early-warning)",
			sig: Signals{
				Now: base, LastPromptKind: PromptStart, LastPromptAt: ago(5 * time.Second),
				LastOutput: ago(5 * time.Second),
			},
			want:     StatusWaitingForInput,
			wantConf: ConfHint,
		},
		{
			name: "prompt mark but output still flowing → not waiting yet",
			sig: Signals{
				Now: base, LastPromptKind: PromptStart, LastPromptAt: ago(5 * time.Second),
				LastOutput: base, // just produced output
			},
			want:     StatusWorking, // recent output
			wantConf: ConfHint,
		},
		{
			name: "command executing + flowing output → working",
			sig: Signals{
				Now: base, LastPromptKind: PromptCommandExecuted, LastPromptAt: ago(2 * time.Second),
				LastOutput: ago(1 * time.Second),
			},
			want:     StatusWorking,
			wantConf: ConfHint,
		},
		{
			name:     "no output for a long time → idle",
			sig:      Signals{Now: base, LastOutput: ago(10 * time.Minute)},
			want:     StatusIdle,
			wantConf: ConfHint,
		},
		{
			name:     "recent output, no marks → working",
			sig:      Signals{Now: base, LastOutput: ago(1 * time.Second)},
			want:     StatusWorking,
			wantConf: ConfHint,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.sig, Thresholds{})
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (evidence=%q)", got.Status, tc.want, got.Evidence)
			}
			if got.Confidence != tc.wantConf {
				t.Errorf("confidence = %q, want %q", got.Confidence, tc.wantConf)
			}
			if got.Evidence == "" {
				t.Error("evidence must never be empty")
			}
		})
	}
}

func TestUntrustedNeverOutranksTrusted(t *testing.T) {
	// A fresh prompt-mark "waiting" hint must NOT override a trusted pre_tool
	// "working" hook — trusted evidence is the anchor (§2.1b).
	s := Signals{
		Now:          base,
		LastHookKind: HookPreTool, LastHookAt: ago(2 * time.Second),
		LastPromptKind: PromptStart, LastPromptAt: ago(1 * time.Second),
		LastOutput: ago(30 * time.Second),
	}
	got := Classify(s, Thresholds{})
	if got.Status != StatusWorking || got.Confidence != ConfTrusted {
		t.Fatalf("trusted hook should win: got %+v", got)
	}
}

func TestAgeReported(t *testing.T) {
	s := Signals{Now: base, LastOutput: ago(10 * time.Minute)}
	got := Classify(s, Thresholds{})
	if got.Status != StatusIdle {
		t.Fatalf("want idle, got %q", got.Status)
	}
	if got.AgeSeconds < 599 || got.AgeSeconds > 601 {
		t.Errorf("age = %v, want ~600", got.AgeSeconds)
	}
}
