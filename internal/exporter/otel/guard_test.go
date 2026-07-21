package otel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
)

// guardFixture is a fully-populated guard event.
func guardFixture() GuardEvent {
	return GuardEvent{
		ID: 9, TS: time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC),
		SessionID: "sess-1", Tool: "claude-code", EventKind: "shell_exec",
		RuleID: "R-101", Category: "destructive", Severity: "critical",
		Decision: "deny", DegradedFrom: "", Enforced: true, Source: "builtin",
		TargetHash: "aabb", ChainHash: "cc11",
		Reason: "recursive delete", TargetExcerpt: "rm -rf /", TaintOrigin: "",
	}
}

func TestGuardSpanName(t *testing.T) {
	t.Parallel()
	if got := GuardSpanName(guardFixture()); got != "guard R-101" {
		t.Errorf("GuardSpanName = %q, want %q", got, "guard R-101")
	}
	if got := GuardSpanName(GuardEvent{}); got != "guard" {
		t.Errorf("GuardSpanName(empty) = %q, want %q", got, "guard")
	}
}

// TestExportGuardEvent pins the span shape: zero duration at the
// event timestamp, the sbo.guard.* attribute set, Error status on
// deny, and the content gate default-off.
func TestExportGuardEvent(t *testing.T) {
	t.Parallel()
	e, inMem := newTestExporter(Config{}, &fakeSource{}, &fakeResolver{})
	e.ExportGuardEvent(context.Background(), guardFixture())
	spans := inMem.GetSpans()
	_ = e.Stop(context.Background())
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name != "guard R-101" {
		t.Errorf("span name = %q", s.Name)
	}
	if !s.StartTime.Equal(s.EndTime) {
		t.Errorf("guard spans must be zero-duration: %v → %v", s.StartTime, s.EndTime)
	}
	if !s.StartTime.Equal(guardFixture().TS) {
		t.Errorf("span time = %v, want the event time", s.StartTime)
	}
	if s.Status.Code != codes.Error {
		t.Errorf("deny span status = %v, want Error", s.Status.Code)
	}

	m := attrMap(s.Attributes)
	for key, want := range map[string]string{
		keySBOGuardRuleID:     "R-101",
		keySBOGuardCategory:   "destructive",
		keySBOGuardSeverity:   "critical",
		keySBOGuardDecision:   "deny",
		keySBOGuardSource:     "builtin",
		keySBOGuardEventKind:  "shell_exec",
		keySBOGuardTargetHash: "aabb",
		keySBOGuardChainHash:  "cc11",
		keySBOSessionID:       "sess-1",
		keySBOToolAdapter:     "claude-code",
	} {
		if got := m[key].AsString(); got != want {
			t.Errorf("attr %s = %q, want %q", key, got, want)
		}
	}
	if m[keySBOGuardEventID].AsInt64() != 9 {
		t.Errorf("event id attr = %v", m[keySBOGuardEventID])
	}
	if !m[keySBOGuardEnforced].AsBool() {
		t.Error("enforced attr must be true")
	}

	// Content gate OFF by default: reason / excerpt / taint absent.
	for _, key := range []string{keySBOGuardReason, keySBOGuardTargetExcerpt, keySBOGuardTaintOrigin} {
		if _, present := m[key]; present {
			t.Errorf("content attr %s must be absent when EmitPromptContent=false", key)
		}
	}
}

// TestExportGuardEvent_ContentGateAndStatus pins the EmitPromptContent
// branch and the non-blocking status table.
func TestExportGuardEvent_ContentGateAndStatus(t *testing.T) {
	t.Parallel()
	e, inMem := newTestExporter(Config{EmitPromptContent: true}, &fakeSource{}, &fakeResolver{})

	cases := []struct {
		decision   string
		wantStatus codes.Code
	}{
		{"deny", codes.Error},
		{"mask", codes.Error},
		{"flag", codes.Unset},
		{"ask", codes.Unset},
		{"allow", codes.Unset},
	}
	for _, c := range cases {
		ev := guardFixture()
		ev.Decision = c.decision
		e.ExportGuardEvent(context.Background(), ev)
	}
	spans := inMem.GetSpans()
	_ = e.Stop(context.Background())
	if len(spans) != len(cases) {
		t.Fatalf("got %d spans, want %d", len(spans), len(cases))
	}
	for i, c := range cases {
		if spans[i].Status.Code != c.wantStatus {
			t.Errorf("decision %q: status = %v, want %v", c.decision, spans[i].Status.Code, c.wantStatus)
		}
	}

	// Content present under the gate.
	m := attrMap(spans[0].Attributes)
	if m[keySBOGuardReason].AsString() != "recursive delete" {
		t.Errorf("reason attr = %q, want emitted under EmitPromptContent", m[keySBOGuardReason].AsString())
	}
	if m[keySBOGuardTargetExcerpt].AsString() != "rm -rf /" {
		t.Errorf("excerpt attr = %q", m[keySBOGuardTargetExcerpt].AsString())
	}
}
