package transcriptutil

import (
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func ts(min int) time.Time { return time.Date(2026, 7, 4, 10, min, 0, 0, time.UTC) }

// TestBuilder_ExchangeMerging pins D-P0.3: consecutive assistant records
// merge into one exchange per user prompt; results attach by id.
func TestBuilder_ExchangeMerging(t *testing.T) {
	b := New()
	b.User("build it", ts(0))
	b.AssistantText("part one", "m1", ts(1))
	b.AssistantCall("c1", "Edit", `{"f":"a.go"}`, "m1", ts(2))
	b.Resolve("c1", "ok", time.Time{})
	b.AssistantText("part two", "", ts(3))
	b.User("next", ts(4))
	b.AssistantCall("c2", "Bash", "go test", "m2", ts(5))
	msgs := b.Finish()

	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (u a u a)", len(msgs))
	}
	a := msgs[1]
	if a.Role != models.TranscriptAssistant || a.Model != "m1" || a.Time != ts(3) {
		t.Errorf("exchange = %+v (time must be the LAST constituent record)", a)
	}
	if !strings.Contains(a.Text, "part one") || !strings.Contains(a.Text, "part two") {
		t.Errorf("merged text = %q", a.Text)
	}
	if len(a.ToolCalls) != 1 || !a.ToolCalls[0].Resolved || a.ToolCalls[0].ResultExcerpt != "ok" {
		t.Errorf("tool call = %+v", a.ToolCalls)
	}
	if msgs[3].ToolCalls[0].Resolved {
		t.Error("dangling call must stay unresolved")
	}
	for i, m := range msgs {
		if m.Index != i {
			t.Errorf("index %d = %d", i, m.Index)
		}
	}
}

// TestBuilder_ResolveTimeSemantics pins the two Resolve time behaviors:
// zero ts leaves the exchange time (claude-code), non-zero refreshes it
// (codex).
func TestBuilder_ResolveTimeSemantics(t *testing.T) {
	b := New()
	b.AssistantCall("c1", "Edit", "{}", "", ts(1))
	b.Resolve("c1", "ok", ts(9))
	if got := b.Finish()[0].Time; got != ts(9) {
		t.Errorf("time = %v, want refreshed to ts(9)", got)
	}

	b2 := New()
	b2.AssistantCall("c1", "Edit", "{}", "", ts(1))
	b2.Resolve("c1", "ok", time.Time{})
	if got := b2.Finish()[0].Time; got != ts(1) {
		t.Errorf("time = %v, want unchanged ts(1)", got)
	}
}

// TestBuilder_ResolveAll pins the id-less settle path (formats that never
// record results): calls settle with empty excerpts, nothing fabricated.
func TestBuilder_ResolveAll(t *testing.T) {
	b := New()
	b.AssistantCall("", "Shell", "ls", "", ts(1))
	b.AssistantCall("", "Read", "f.go", "", ts(2))
	b.ResolveAll()
	b.AssistantCall("", "Edit", "{}", "", ts(3)) // after the settle point
	msgs := b.Finish()
	calls := msgs[0].ToolCalls
	if !calls[0].Resolved || !calls[1].Resolved || calls[2].Resolved {
		t.Errorf("resolved = %v/%v/%v, want true/true/false", calls[0].Resolved, calls[1].Resolved, calls[2].Resolved)
	}
	if calls[0].ResultExcerpt != "" {
		t.Errorf("settled excerpt = %q, want empty (never fabricated)", calls[0].ResultExcerpt)
	}
}

// TestBuilder_CapsAndEmpties pins the excerpt caps and empty-input drops.
func TestBuilder_CapsAndEmpties(t *testing.T) {
	b := New()
	b.User("  ", ts(0)) // dropped
	b.User(strings.Repeat("x", TextCap+10), ts(1))
	b.AssistantText("  ", "", ts(2)) // dropped
	b.AssistantCall("c1", "Edit", strings.Repeat("i", InputCap+10), "", ts(3))
	b.Resolve("c1", strings.Repeat("r", ResultCap+10), time.Time{})
	msgs := b.Finish()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if !msgs[0].Truncated || len(msgs[0].Text) > TextCap+len("…") {
		t.Errorf("text not capped: %d", len(msgs[0].Text))
	}
	call := msgs[1].ToolCalls[0]
	if len(call.InputExcerpt) > InputCap+len("…") || len(call.ResultExcerpt) > ResultCap+len("…") {
		t.Errorf("excerpts not capped: %d/%d", len(call.InputExcerpt), len(call.ResultExcerpt))
	}
}

// TestBuilder_ParallelCallBatchResolves pins the aliasing regression: a
// batch of calls appended BEFORE any result arrives (parallel tool use)
// must all resolve — pointers into an append-grown slice go stale on
// reallocation, which silently dropped early resolves in the P1
// per-adapter builders.
func TestBuilder_ParallelCallBatchResolves(t *testing.T) {
	b := New()
	ids := []string{"c1", "c2", "c3", "c4", "c5"}
	for i, id := range ids {
		b.AssistantCall(id, "Read", "{}", "", ts(i))
	}
	for _, id := range ids {
		b.Resolve(id, "ok "+id, time.Time{})
	}
	calls := b.Finish()[0].ToolCalls
	if len(calls) != len(ids) {
		t.Fatalf("calls = %d, want %d", len(calls), len(ids))
	}
	for i, c := range calls {
		if !c.Resolved || c.ResultExcerpt != "ok "+ids[i] {
			t.Errorf("call %s: resolved=%v excerpt=%q", ids[i], c.Resolved, c.ResultExcerpt)
		}
	}
}
