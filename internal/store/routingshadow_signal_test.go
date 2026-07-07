package store

import (
	"testing"
	"time"
)

// TestAdviseShadowSignal pins the X3.1 composite: an empty window
// early-outs to a not-ready zero report (no modelvalue load), and with
// advise history the gate composes through BuildAdviseShadowReport —
// reroutes without parity evidence flag, so Ready stays false (maximum
// caution; the gate logic itself is pinned by the shadow-report tests).
func TestAdviseShadowSignal(t *testing.T) {
	t.Parallel()
	st, ctx := openRoutingTestStore(t)

	rep, err := st.AdviseShadowSignal(ctx, 14, nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if rep.AdviseDecisions != 0 || rep.ReadyToPromote || rep.MinDecisions == 0 {
		t.Fatalf("empty signal = %+v, want zero not-ready with the floor carried", rep)
	}

	now := time.Now().UTC()
	rows := []RouterDecisionRow{
		{
			SessionID: "sA", Timestamp: now.Add(-time.Hour), Mode: "advise", Channel: "B",
			OriginalModel: "claude-opus-4-8", SelectedModel: "claude-haiku-4-5",
			TurnKind: "read_only", PolicyName: "value", PolicyHash: "h",
			ReasonCodes: []string{"overpowered_read"}, EstSavingsUSD: 0.4, EstimateVersion: "p1-v1",
		},
		{
			SessionID: "sA", Timestamp: now.Add(-2 * time.Hour), Mode: "advise", Channel: "B",
			OriginalModel: "claude-opus-4-8", SelectedModel: "claude-opus-4-8",
			TurnKind: "plan", PolicyName: "value", PolicyHash: "h",
			ReasonCodes: []string{"no_route"}, EstimateVersion: "p1-v1",
		},
	}
	if err := st.InsertRouterDecisions(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep, err = st.AdviseShadowSignal(ctx, 14, nil)
	if err != nil {
		t.Fatalf("seeded: %v", err)
	}
	if rep.AdviseDecisions != 2 || rep.WouldReroute != 1 {
		t.Fatalf("signal = %+v, want 2 advise decisions / 1 reroute", rep)
	}
	if rep.QualityFlags != 1 {
		t.Errorf("reroute without parity evidence must flag (maximum caution): %+v", rep)
	}
	if rep.ReadyToPromote {
		t.Error("2 decisions with a flag must not read as ready")
	}
}
