package cowork

import (
	"context"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestToolUseSummary_AttachesToMatchingToolUse pins that a
// tool_use_summary record stamps its summary text onto the matching
// tool_use action's Metadata.CoworkToolSummary via the
// preceding_tool_use_ids[0] join.
func TestToolUseSummary_AttachesToMatchingToolUse(t *testing.T) {
	t.Parallel()
	root := fixturePath(t, "")
	auditPath := fixturePath(t, "cowork-aaaa/dev-bbbb/local_cccc-dddd-eeee/audit.jsonl")
	a := NewWithOptions(nil, root)

	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	var read *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].RawToolName == "Read" {
			read = &res.ToolEvents[i]
			break
		}
	}
	if read == nil {
		t.Fatalf("no Read tool_use event in %#v", summarizeEvents(res.ToolEvents))
	}
	if read.Metadata == nil {
		t.Fatalf("Read.Metadata nil — tool_use_summary join didn't stamp")
	}
	if read.Metadata.CoworkToolSummary != "Read foo.go from the workspace" {
		t.Fatalf("CoworkToolSummary=%q want %q", read.Metadata.CoworkToolSummary, "Read foo.go from the workspace")
	}
}

// TestRateLimitEvent_EmitsActionRateLimit pins that a rate_limit_event
// record emits exactly one ActionRateLimit row with the full
// rate_limit_info payload broken out into typed metadata fields.
func TestRateLimitEvent_EmitsActionRateLimit(t *testing.T) {
	t.Parallel()
	root := fixturePath(t, "")
	auditPath := fixturePath(t, "cowork-aaaa/dev-bbbb/local_cccc-dddd-eeee/audit.jsonl")
	a := NewWithOptions(nil, root)

	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	var rl *models.ToolEvent
	count := 0
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionRateLimit {
			rl = &res.ToolEvents[i]
			count++
		}
	}
	if count != 1 || rl == nil {
		t.Fatalf("ActionRateLimit count=%d want 1", count)
	}
	if rl.Target != "five_hour" {
		t.Fatalf("Target=%q want %q", rl.Target, "five_hour")
	}
	if rl.RawToolName != "allowed" {
		t.Fatalf("RawToolName=%q want %q", rl.RawToolName, "allowed")
	}
	if !rl.Success {
		t.Fatalf("Success=false; want true for status=allowed")
	}
	if rl.Metadata == nil {
		t.Fatalf("Metadata nil — rate-limit fields not stamped")
	}
	if rl.Metadata.RateLimitStatus != "allowed" {
		t.Fatalf("RateLimitStatus=%q", rl.Metadata.RateLimitStatus)
	}
	if rl.Metadata.RateLimitType != "five_hour" {
		t.Fatalf("RateLimitType=%q", rl.Metadata.RateLimitType)
	}
	if rl.Metadata.RateLimitResetsAt != 1772546400 {
		t.Fatalf("RateLimitResetsAt=%d", rl.Metadata.RateLimitResetsAt)
	}
	if rl.Metadata.RateLimitOverageStatus != "rejected" {
		t.Fatalf("RateLimitOverageStatus=%q", rl.Metadata.RateLimitOverageStatus)
	}
}

// TestSidechain_CrossReferencedFromSubagentFiles pins that an
// audit.jsonl record whose uuid appears in any inner subagent
// transcript gets IsSidechain=true on its emitted action.
//
// Fixture has subagents/agent-fixture.jsonl listing assistant uuid
// "uasst-sidechain". The audit.jsonl's assistant record with that
// same uuid emits an assistant_text event — that event MUST have
// IsSidechain=true. The other assistant record (uasst-0001) MUST
// have IsSidechain=false (its uuid isn't in the subagent file).
func TestSidechain_CrossReferencedFromSubagentFiles(t *testing.T) {
	t.Parallel()
	root := fixturePath(t, "")
	auditPath := fixturePath(t, "cowork-aaaa/dev-bbbb/local_cccc-dddd-eeee/audit.jsonl")
	a := NewWithOptions(nil, root)

	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// uasst-0001's assistant_text event — source event ID starts with uasst-0001.
	// uasst-sidechain's assistant_text event — source event ID starts with uasst-sidechain.
	var mainAsst, sidechainAsst *models.ToolEvent
	for i := range res.ToolEvents {
		ev := &res.ToolEvents[i]
		if ev.ActionType != models.ActionAssistantMessage {
			continue
		}
		if ev.RawToolName != "cowork.assistant_text" {
			continue
		}
		switch {
		case startsWith(ev.SourceEventID, "uasst-0001:"):
			mainAsst = ev
		case startsWith(ev.SourceEventID, "uasst-sidechain:"):
			sidechainAsst = ev
		}
	}
	if mainAsst == nil || sidechainAsst == nil {
		t.Fatalf("missing assistant events: main=%v sidechain=%v", mainAsst, sidechainAsst)
	}
	if mainAsst.IsSidechain {
		t.Errorf("mainAsst.IsSidechain=true; want false (uuid not in subagent file)")
	}
	if !sidechainAsst.IsSidechain {
		t.Errorf("sidechainAsst.IsSidechain=false; want true (uuid IS in subagent file)")
	}
}

// TestSidechain_NoSubagentFilesYieldsEmptySet pins that an
// instance dir with no .claude/projects/ subtree returns an empty
// sidechain set (no actions flagged).
func TestSidechain_NoSubagentFilesYieldsEmptySet(t *testing.T) {
	t.Parallel()
	got := collectSidechainUUIDs("/nonexistent/instance/dir")
	if len(got) != 0 {
		t.Fatalf("collectSidechainUUIDs(missing)=%v want empty", got)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
