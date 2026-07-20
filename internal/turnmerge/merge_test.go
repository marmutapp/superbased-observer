package turnmerge

import (
	"reflect"
	"testing"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name        string
		existing    *Turn
		incoming    Turn
		wantAction  Action
		wantChanged []string
		// check is an optional extra assertion on the merged Turn.
		check func(t *testing.T, got Turn)
	}{
		{
			name:       "first observation inserts as-is (native-only, no proxy)",
			existing:   nil,
			incoming:   Turn{RequestID: "req-1", Fidelity: FidelityNativeExact, InputTokens: 100, OutputTokens: 20, StopReason: "end_turn"},
			wantAction: ActionInsert,
			check: func(t *testing.T, got Turn) {
				// Gate 2: a native-only turn lands at full fidelity.
				if got.InputTokens != 100 || got.StopReason != "end_turn" || got.Fidelity != FidelityNativeExact {
					t.Fatalf("native-only insert lost fields: %+v", got)
				}
			},
		},
		{
			name: "proxy wins tokens, native fills enrichment (acceptance gate 1)",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact,
				InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50, CostUSD: 0.42,
				// proxy lacked ttft + stop_reason
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityNativeExact,
				InputTokens: 999, OutputTokens: 199, // lower-fidelity token numbers — must NOT win
				TimeToFirstTokenMS: 320, StopReason: "end_turn",
			},
			wantAction:  ActionUpdate,
			wantChanged: []string{"time_to_first_token_ms", "stop_reason"},
			check: func(t *testing.T, got Turn) {
				if got.InputTokens != 1000 || got.OutputTokens != 200 || got.CostUSD != 0.42 {
					t.Fatalf("lower-fidelity native overwrote proxy tokens: %+v", got)
				}
				if got.TimeToFirstTokenMS != 320 || got.StopReason != "end_turn" {
					t.Fatalf("native enrichment not merged in: %+v", got)
				}
				if got.Fidelity != FidelityProxyExact {
					t.Fatalf("fidelity downgraded to %v", got.Fidelity)
				}
			},
		},
		{
			name: "higher-fidelity proxy overwrites approximate jsonl tokens",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityApprox,
				InputTokens: 900, OutputTokens: 180,
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact,
				InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50, CostUSD: 0.42,
			},
			wantAction:  ActionUpdate,
			wantChanged: []string{"input_tokens", "output_tokens", "cache_read_tokens", "cost_usd", "fidelity"},
			check: func(t *testing.T, got Turn) {
				if got.InputTokens != 1000 || got.CacheReadTokens != 50 || got.Fidelity != FidelityProxyExact {
					t.Fatalf("proxy did not win over approx: %+v", got)
				}
			},
		},
		{
			name: "equal-fidelity snapshot re-emit takes the MAX",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityApprox,
				InputTokens: 100, OutputTokens: 10,
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityApprox,
				InputTokens: 150, OutputTokens: 5, // refined input up, output reported lower
			},
			wantAction:  ActionUpdate,
			wantChanged: []string{"input_tokens"},
			check: func(t *testing.T, got Turn) {
				// MAX-upgrade: input climbs to 150, output stays at the higher 10.
				if got.InputTokens != 150 || got.OutputTokens != 10 {
					t.Fatalf("max-upgrade wrong: %+v", got)
				}
			},
		},
		{
			name: "lower-fidelity incoming adds nothing -> no change",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact,
				InputTokens: 1000, OutputTokens: 200, TimeToFirstTokenMS: 100, StopReason: "end_turn",
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityApprox,
				InputTokens: 1, TimeToFirstTokenMS: 999, StopReason: "max_tokens",
			},
			wantAction: ActionNoChange,
		},
		{
			name: "enrichment never overwrites an existing non-zero value",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact,
				TimeToFirstTokenMS: 100, StopReason: "end_turn",
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityNativeExact,
				TimeToFirstTokenMS: 320, StopReason: "tool_use",
			},
			wantAction: ActionNoChange,
		},
		{
			name: "equal-fidelity cost takes the MAX",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact, CostUSD: 0.40,
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact, CostUSD: 0.55,
			},
			wantAction:  ActionUpdate,
			wantChanged: []string{"cost_usd"},
			check: func(t *testing.T, got Turn) {
				if got.CostUSD != 0.55 {
					t.Fatalf("cost max-upgrade wrong: %v", got.CostUSD)
				}
			},
		},
		{
			name: "pure fidelity bump with identical data still updates provenance",
			existing: &Turn{
				RequestID: "req-1", Fidelity: FidelityNativeExact,
				InputTokens: 1000, OutputTokens: 200,
			},
			incoming: Turn{
				RequestID: "req-1", Fidelity: FidelityProxyExact,
				InputTokens: 1000, OutputTokens: 200,
			},
			wantAction:  ActionUpdate,
			wantChanged: []string{"fidelity"},
		},
	}

	for a, want := range map[Action]string{ActionInsert: "insert", ActionUpdate: "update", ActionNoChange: "no-change", Action(99): "unknown"} {
		if got := a.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", a, got, want)
		}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Guard against input mutation: keep a copy to compare after.
			var existingCopy *Turn
			if tc.existing != nil {
				c := *tc.existing
				existingCopy = &c
			}
			incomingCopy := tc.incoming

			got := Merge(tc.existing, tc.incoming)

			if got.Action != tc.wantAction {
				t.Errorf("Action = %v, want %v", got.Action, tc.wantAction)
			}
			if tc.wantChanged != nil && !reflect.DeepEqual(got.Changed, tc.wantChanged) {
				t.Errorf("Changed = %v, want %v", got.Changed, tc.wantChanged)
			}
			if tc.check != nil {
				tc.check(t, got.Turn)
			}
			if tc.existing != nil && !reflect.DeepEqual(*tc.existing, *existingCopy) {
				t.Errorf("Merge mutated existing input: got %+v want %+v", *tc.existing, *existingCopy)
			}
			if !reflect.DeepEqual(tc.incoming, incomingCopy) {
				t.Errorf("Merge mutated incoming input")
			}
		})
	}
}
