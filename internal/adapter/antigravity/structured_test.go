package antigravity

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// TestParseStructuredTrajectory_Synthetic verifies the parser walks
// our hand-crafted protobuf shape without needing a real bridge
// fetch. Always runs.
func TestParseStructuredTrajectory_Synthetic(t *testing.T) {
	// Build: 1 (Trajectory) → 1.3 (turn) × 2 with first turn carrying
	// 1.3.3.28 (model) inline (matches FB48 shape: model meta lives
	// inside the first turn's .3 metadata wrapper) + 1.2.5.1.1
	// (per-step timestamp).
	// turn 0: 50 response + 12 reasoning = 62 total output emission (.3).
	// turn 1: 75 response + 0 reasoning (Claude-style, .9 absent).
	turn1Body := buildTurn(t, 100, 200, 50, 1000, 12) // input, cacheCreation, output(.10), cacheRead, reasoning(.9)
	turn2Body := buildTurn(t, 100, 0, 75, 2000, 0)

	// Append the model meta inside turn1 so the path resolves to
	// [1,3,3,28], not [1,3,28].
	modelMetaBody := protowire.AppendBytesField(nil, 28, []byte("claude-sonnet-4-5"))
	turn1Body = append(turn1Body, protowire.AppendBytesField(nil, 3, modelMetaBody)...)

	// Three steps: span 100s and a file-view tool call on the middle one.
	step1 := buildStep(t, 1769687860, "")
	step2 := buildStep(t, 1769687900, "file:///c:/programsx/regulation/main.go") // file view
	step3 := buildStep(t, 1769687960, "")

	turn1 := protowire.AppendBytesField(nil, 3, turn1Body) // 1.3
	turn2 := protowire.AppendBytesField(nil, 3, turn2Body) // 1.3

	wrapperBody := append(append(append(append(step1, step2...), step3...), turn1...), turn2...)
	wire := protowire.AppendBytesField(nil, 1, wrapperBody) // 1 (Trajectory)

	got := ParseStructuredTrajectory(wire, "uuid-test", "/tmp/proj", "/tmp/uuid-test.pb", nil)

	if got.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want claude-sonnet-4-5", got.Model)
	}
	wantStart := time.Unix(1769687860, 0).UTC()
	wantEnd := time.Unix(1769687960, 0).UTC()
	if !got.StartedAt.Equal(wantStart) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, wantStart)
	}
	if !got.EndedAt.Equal(wantEnd) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, wantEnd)
	}
	if len(got.TokenEvents) != 2 {
		t.Fatalf("TokenEvents count = %d, want 2", len(got.TokenEvents))
	}
	if got.TokenEvents[0].InputTokens != 100 ||
		got.TokenEvents[0].CacheCreationTokens != 200 ||
		got.TokenEvents[0].OutputTokens != 50 ||
		got.TokenEvents[0].CacheReadTokens != 1000 ||
		got.TokenEvents[0].ReasoningTokens != 12 {
		t.Errorf("turn 0 tokens = %+v, want input=100 cc=200 output=50 cr=1000 reasoning=12", got.TokenEvents[0])
	}
	if got.TokenEvents[1].CacheReadTokens != 2000 {
		t.Errorf("turn 1 cache_read = %d, want 2000", got.TokenEvents[1].CacheReadTokens)
	}
	if got.TokenEvents[1].ReasoningTokens != 0 {
		t.Errorf("turn 1 reasoning = %d, want 0 (Claude-style: .9 absent)", got.TokenEvents[1].ReasoningTokens)
	}
	if got.TokenEvents[0].Model != "claude-sonnet-4-5" {
		t.Errorf("turn 0 Model = %q, want claude-sonnet-4-5", got.TokenEvents[0].Model)
	}
	if got.TokenEvents[0].SessionID != "uuid-test" {
		t.Errorf("turn 0 SessionID = %q, want uuid-test", got.TokenEvents[0].SessionID)
	}
	if got.TokenEvents[0].SourceEventID == got.TokenEvents[1].SourceEventID {
		t.Error("SourceEventID must differ across turns to avoid the (source_file, source_event_id) UNIQUE collapsing rows")
	}
	// Spread across [start,end] window: turn 0 at start, turn 1 at end.
	if !got.TokenEvents[0].Timestamp.Equal(wantStart) {
		t.Errorf("turn 0 ts = %v, want %v (first event sits at StartedAt)", got.TokenEvents[0].Timestamp, wantStart)
	}
	if !got.TokenEvents[1].Timestamp.Equal(wantEnd) {
		t.Errorf("turn 1 ts = %v, want %v (last event sits at EndedAt)", got.TokenEvents[1].Timestamp, wantEnd)
	}
	// MessageID scheme: shared between TokenEvents and ToolEvents.
	wantTurn0MsgID := "antigravity:uuid-test:turn:0"
	if got.TokenEvents[0].MessageID != wantTurn0MsgID {
		t.Errorf("turn 0 MessageID = %q, want %q", got.TokenEvents[0].MessageID, wantTurn0MsgID)
	}
	// ToolEvents: middle step (index 1) carries a file-view, with
	// duration_ms = (step3.ts - step2.ts) * 1000 = 60_000.
	if len(got.ToolEvents) != 1 {
		t.Fatalf("ToolEvents count = %d, want 1", len(got.ToolEvents))
	}
	te := got.ToolEvents[0]
	if te.ActionType != "read_file" {
		t.Errorf("tool ActionType = %q, want read_file", te.ActionType)
	}
	if te.Target != "c:/programsx/regulation/main.go" {
		t.Errorf("tool Target = %q, want decoded file path", te.Target)
	}
	if te.DurationMs != 60_000 {
		t.Errorf("tool DurationMs = %d, want 60000 (=60s gap to next step in ms)", te.DurationMs)
	}
	wantToolTs := time.Unix(1769687900, 0).UTC()
	if !te.Timestamp.Equal(wantToolTs) {
		t.Errorf("tool Timestamp = %v, want %v (real per-step ts)", te.Timestamp, wantToolTs)
	}
	// Step 1 of 3 → assignedTurn = floor(1 * 2 / 3) = 0.
	if te.MessageID != wantTurn0MsgID {
		t.Errorf("tool MessageID = %q, want %q (joins to parent turn so dashboard's Tools count works)", te.MessageID, wantTurn0MsgID)
	}
}

// TestParseStructuredTrajectory_RealFB48 verifies the parser against
// the live language_server response captured via the bridge. Skipped
// unless ANTIGRAVITY_PROBE_FILE is set. Use:
//
//	./bin/antigravity-bridge.exe structured fb48b020-3513-4298-8ea2-bbce3756bd31 > /tmp/probe-getct.bin
//	ANTIGRAVITY_PROBE_FILE=/tmp/probe-getct.bin go test -run TestParseStructuredTrajectory_RealFB48 -v ./internal/adapter/antigravity/
func TestParseStructuredTrajectory_RealFB48(t *testing.T) {
	path := os.Getenv("ANTIGRAVITY_PROBE_FILE")
	if path == "" {
		t.Skip("set ANTIGRAVITY_PROBE_FILE=/tmp/probe-getct.bin to run")
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := ParseStructuredTrajectory(buf, "fb48b020-3513-4298-8ea2-bbce3756bd31", "/mnt/c/programsx/regulation", path, nil)
	if got.Model == "" {
		t.Error("Model empty — verification path is broken (1.3.3.28 should yield claude-sonnet-4-5)")
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt zero — verification path is broken (1.2.5.1.1 should yield non-zero)")
	}
	if len(got.TokenEvents) == 0 {
		t.Fatal("no TokenEvents extracted — verification path is broken")
	}
	t.Logf("model=%s started_at=%s ended_at=%s turns=%d tools=%d",
		got.Model, got.StartedAt.Format(time.RFC3339), got.EndedAt.Format(time.RFC3339),
		len(got.TokenEvents), len(got.ToolEvents))
	for i, te := range got.TokenEvents {
		t.Logf("  turn %d @%s: input=%d cache_creation=%d output=%d cache_read=%d reasoning=%d",
			i, te.Timestamp.Format("15:04:05"),
			te.InputTokens, te.CacheCreationTokens, te.OutputTokens, te.CacheReadTokens, te.ReasoningTokens)
	}
	var artifactEdits, artifactWrites, fileViews, userPrompts, assistantTexts, runCommands, planSteps, finalSummaries int
	for _, te := range got.ToolEvents {
		switch te.RawToolName {
		case "structured.artifact_edit":
			artifactEdits++
		case "structured.artifact_write":
			artifactWrites++
		case "structured.file_view":
			fileViews++
		case "structured.user_prompt":
			userPrompts++
		case "structured.assistant_text":
			assistantTexts++
		case "structured.run_command":
			runCommands++
		case "structured.plan_step":
			planSteps++
		case "structured.final_summary":
			finalSummaries++
		}
	}
	t.Logf("  tool breakdown: file_view=%d artifact_edit=%d artifact_write=%d user_prompt=%d assistant_text=%d run_command=%d plan_step=%d final_summary=%d",
		fileViews, artifactEdits, artifactWrites, userPrompts, assistantTexts, runCommands, planSteps, finalSummaries)
	if userPrompts < 1 {
		t.Errorf("no structured.user_prompt rows extracted from FB48 — Tier 2 walker is broken (1.2.19.2 should yield 3)")
	}
	if assistantTexts < 1 {
		t.Errorf("no structured.assistant_text rows extracted from FB48 — Tier 3 walker is broken (1.2.20.1 should yield ~29)")
	}
	for i, te := range got.ToolEvents {
		t.Logf("  tool %d @%s: %s target=%q raw=%s duration_ms=%d msg=%s",
			i, te.Timestamp.Format("15:04:05"), te.ActionType, te.Target, te.RawToolName, te.DurationMs, te.MessageID)
	}
	// Tier 1 acceptance: FB48 has 9 1.2.10 artifact entries (3 Flavor
	// A diff edits + 6 Flavor B full-doc snapshots). Probe-confirmed
	// 2026-05-04. If the trajectory wire format drifts these counts
	// will shift; the test is informational past >=1.
	if artifactEdits+artifactWrites < 1 {
		t.Errorf("no artifact ToolEvents extracted from FB48 — Tier 1 walker is broken")
	}
	if artifactEdits+artifactWrites != 9 {
		t.Logf("note: FB48 baseline = 9 artifact events (3 edit + 6 write); got %d edit + %d write",
			artifactEdits, artifactWrites)
	}
}

func buildTurn(t *testing.T, input, cacheCreation, output, cacheRead, reasoning uint64) []byte {
	t.Helper()
	// Build 1.3[].1.17.2 with sub-fields:
	//   .1  = input_tokens
	//   .2  = cache_creation_input_tokens
	//   .3  = total output (output + reasoning) — wire-format leftover
	//         we no longer read; preserved so the fixture matches the
	//         empirical envelope shape on real sessions.
	//   .5  = cache_read_input_tokens
	//   .9  = reasoning_output_tokens (omitted when reasoning==0 to
	//         mirror the Claude-style "field absent" wire shape)
	//   .10 = response_output_tokens (what the parser now maps to
	//         OutputTokens; codex convention is reasoning+output additive)
	usageInner := protowire.AppendVarintField(nil, 1, input)
	usageInner = protowire.AppendVarintField(usageInner, 2, cacheCreation)
	usageInner = protowire.AppendVarintField(usageInner, 3, output+reasoning)
	usageInner = protowire.AppendVarintField(usageInner, 5, cacheRead)
	if reasoning > 0 {
		usageInner = protowire.AppendVarintField(usageInner, 9, reasoning)
	}
	usageInner = protowire.AppendVarintField(usageInner, 10, output)
	usage17_2 := protowire.AppendBytesField(nil, 2, usageInner) // .17.2
	usage17 := protowire.AppendBytesField(nil, 17, usage17_2)   // .17
	turnInner := protowire.AppendBytesField(nil, 1, usage17)    // .1
	return turnInner
}

// buildStep builds a 1.2[] step entry with timestamp at 1.2.5.1.1
// and an optional file-view tool call at 1.2.14.1.
func buildStep(t *testing.T, ts uint64, fileURI string) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil,
		5, // 1.2.5
		protowire.AppendBytesField(
			nil,
			1,                                       // 1.2.5.1
			protowire.AppendVarintField(nil, 1, ts), // 1.2.5.1.1
		),
	)
	if fileURI != "" {
		stepBody = append(
			stepBody,
			protowire.AppendBytesField(
				nil, 14, // 1.2.14
				protowire.AppendBytesField(nil, 1, []byte(fileURI)), // 1.2.14.1
			)...,
		)
	}
	return protowire.AppendBytesField(nil, 2, stepBody) // 1.2 → step
}

// buildArtifactStepFlavorA builds a 1.2[] step entry with a 1.2.10
// artifact diff-style edit (Flavor A) — task description, brain
// artifact URI, workspace dir + basename, before/after body.
func buildArtifactStepFlavorA(t *testing.T, ts uint64, desc, brainURI, workURI, basename, before, after string) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil, 5, // 1.2.5
		protowire.AppendBytesField(
			nil, 1, // 1.2.5.1
			protowire.AppendVarintField(nil, 1, ts), // 1.2.5.1.1
		),
	)
	// 1.2.10.1.1.x — Flavor A.
	inner := protowire.AppendBytesField(nil, 1, []byte(desc)) // 1.2.10.1.1.1
	target46 := protowire.AppendBytesField(nil, 1, []byte(workURI))
	target46 = append(target46, protowire.AppendBytesField(nil, 2, []byte(basename))...)
	target4 := protowire.AppendBytesField(nil, 5, []byte(brainURI))
	target4 = append(target4, protowire.AppendBytesField(nil, 6, target46)...)
	inner = append(inner, protowire.AppendBytesField(nil, 4, target4)...)
	body9 := protowire.AppendBytesField(nil, 1, []byte(before))
	body9 = append(body9, protowire.AppendBytesField(nil, 2, []byte(after))...)
	inner = append(inner, protowire.AppendBytesField(nil, 9, body9)...)
	flavorAWrap := protowire.AppendBytesField(nil, 1, inner)                      // 1.2.10.1.1
	tenInner := protowire.AppendBytesField(nil, 1, flavorAWrap)                   // 1.2.10.1
	stepBody = append(stepBody, protowire.AppendBytesField(nil, 10, tenInner)...) // 1.2.10
	return protowire.AppendBytesField(nil, 2, stepBody)
}

// buildArtifactStepFlavorB builds a 1.2[] step entry with a 1.2.10
// artifact full-document snapshot (Flavor B) — doc URI + full body,
// no diff.
func buildArtifactStepFlavorB(t *testing.T, ts uint64, docURI, docBody string) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil, 5,
		protowire.AppendBytesField(
			nil, 1,
			protowire.AppendVarintField(nil, 1, ts),
		),
	)
	// 1.2.10.1.2.x — Flavor B.
	uriWrap := protowire.AppendBytesField(nil, 5, []byte(docURI)) // 1.2.10.1.2.2.5
	flavorBInner := protowire.AppendBytesField(nil, 1, []byte(docBody))
	flavorBInner = append(flavorBInner, protowire.AppendBytesField(nil, 2, uriWrap)...)
	tenInner := protowire.AppendBytesField(nil, 2, flavorBInner) // 1.2.10.1.2
	tenWrap := protowire.AppendBytesField(nil, 1, tenInner)      // 1.2.10.1
	stepBody = append(stepBody, protowire.AppendBytesField(nil, 10, tenWrap)...)
	return protowire.AppendBytesField(nil, 2, stepBody)
}

// TestParseStructuredTrajectory_ArtifactExtraction (Tier 1) verifies
// 1.2.10 artifact extraction emits one ToolEvent per step, with
// proper Flavor A / Flavor B classification and decoded paths.
func TestParseStructuredTrajectory_ArtifactExtraction(t *testing.T) {
	turn1 := protowire.AppendBytesField(nil, 3, buildTurn(t, 100, 0, 50, 1000, 0)) // 1.3
	step1 := buildArtifactStepFlavorA(
		t,
		1769687860,
		"Refactor parser",
		"file:///C:/Users/u/.gemini/antigravity/brain/uuid/task.md",
		"file:///c:/programsx/regulation",
		"main.go",
		"old contents",
		"new contents",
	)
	step2 := buildArtifactStepFlavorB(
		t,
		1769687920,
		"file:///c:/programsx/regulation/walkthrough.md",
		"# Walkthrough\nFull document body here.",
	)
	wire := protowire.AppendBytesField(nil, 1, append(append(step1, step2...), turn1...))

	got := ParseStructuredTrajectory(wire, "uuid-art", "/tmp/proj", "/tmp/uuid-art.pb", nil)

	if len(got.ToolEvents) != 2 {
		t.Fatalf("ToolEvents count = %d, want 2 (one per artifact step)", len(got.ToolEvents))
	}

	// Step 1 — Flavor A.
	a := got.ToolEvents[0]
	if a.RawToolName != "structured.artifact_edit" {
		t.Errorf("Flavor A RawToolName = %q, want structured.artifact_edit", a.RawToolName)
	}
	if a.ActionType != "edit_file" {
		t.Errorf("Flavor A ActionType = %q, want edit_file", a.ActionType)
	}
	wantTargetA := "c:/programsx/regulation/main.go"
	if a.Target != wantTargetA {
		t.Errorf("Flavor A Target = %q, want %q (workspace dir + basename joined)", a.Target, wantTargetA)
	}
	if a.RawToolInput != "Refactor parser" {
		t.Errorf("Flavor A RawToolInput = %q, want Refactor parser", a.RawToolInput)
	}
	if a.PrecedingReasoning != "old contents" {
		t.Errorf("Flavor A PrecedingReasoning = %q, want old contents (before-edit body)", a.PrecedingReasoning)
	}
	if a.ToolOutput != "new contents" {
		t.Errorf("Flavor A ToolOutput = %q, want new contents (after-edit body)", a.ToolOutput)
	}
	if a.SourceEventID != "antigravity-struct-artifact:uuid-art:step:0" {
		t.Errorf("Flavor A SourceEventID = %q, want antigravity-struct-artifact:uuid-art:step:0", a.SourceEventID)
	}

	// Step 2 — Flavor B.
	b := got.ToolEvents[1]
	if b.RawToolName != "structured.artifact_write" {
		t.Errorf("Flavor B RawToolName = %q, want structured.artifact_write", b.RawToolName)
	}
	wantTargetB := "c:/programsx/regulation/walkthrough.md"
	if b.Target != wantTargetB {
		t.Errorf("Flavor B Target = %q, want %q", b.Target, wantTargetB)
	}
	if b.PrecedingReasoning != "" {
		t.Errorf("Flavor B PrecedingReasoning = %q, want empty (Flavor B has no diff)", b.PrecedingReasoning)
	}
	if !strings.Contains(b.ToolOutput, "Full document body here") {
		t.Errorf("Flavor B ToolOutput should contain doc body, got %q", b.ToolOutput)
	}
	if b.SourceEventID != "antigravity-struct-artifact:uuid-art:step:1" {
		t.Errorf("Flavor B SourceEventID = %q, want antigravity-struct-artifact:uuid-art:step:1", b.SourceEventID)
	}
}

// TestParseStructuredTrajectory_UserPromptExtraction (Tier 2)
// verifies that 1.2.19.2 user-message text is emitted as a
// structured.user_prompt ToolEvent — one per occurrence of 1.2.19.
func TestParseStructuredTrajectory_UserPromptExtraction(t *testing.T) {
	turn1 := protowire.AppendBytesField(nil, 3, buildTurn(t, 100, 0, 50, 1000, 0))
	step := buildUserPromptStep(t, 1769687860, "Refactor the parser to use a state machine.")
	wire := protowire.AppendBytesField(nil, 1, append(step, turn1...))

	got := ParseStructuredTrajectory(wire, "uuid-up", "/tmp/proj", "/tmp/uuid-up.pb", nil)

	if len(got.ToolEvents) != 1 {
		t.Fatalf("ToolEvents count = %d, want 1", len(got.ToolEvents))
	}
	te := got.ToolEvents[0]
	if te.RawToolName != "structured.user_prompt" {
		t.Errorf("RawToolName = %q, want structured.user_prompt", te.RawToolName)
	}
	if te.ActionType != "user_prompt" {
		t.Errorf("ActionType = %q, want user_prompt", te.ActionType)
	}
	if !strings.Contains(te.RawToolInput, "Refactor the parser") {
		t.Errorf("RawToolInput = %q, want to contain prompt text", te.RawToolInput)
	}
	wantSEID := "antigravity-struct-payload:uuid-up:step:0:user"
	if te.SourceEventID != wantSEID {
		t.Errorf("SourceEventID = %q, want %q", te.SourceEventID, wantSEID)
	}
}

// TestParseStructuredTrajectory_AssistantTextExtraction (Tier 3)
// verifies that 1.2.20.1 assistant body text is emitted as a
// structured.assistant_text task_complete ToolEvent. 1.2.20 is the
// PLANNER_RESPONSE step (enum 1.2.1 = 15).
func TestParseStructuredTrajectory_AssistantTextExtraction(t *testing.T) {
	turn1 := protowire.AppendBytesField(nil, 3, buildTurn(t, 100, 0, 50, 1000, 0))
	step := buildAssistantTextStep(t, 1769687860, "I'll analyze the search implementation and propose improvements.")
	wire := protowire.AppendBytesField(nil, 1, append(step, turn1...))

	got := ParseStructuredTrajectory(wire, "uuid-at", "/tmp/proj", "/tmp/uuid-at.pb", nil)

	if len(got.ToolEvents) != 1 {
		t.Fatalf("ToolEvents count = %d, want 1", len(got.ToolEvents))
	}
	te := got.ToolEvents[0]
	if te.RawToolName != "structured.assistant_text" {
		t.Errorf("RawToolName = %q, want structured.assistant_text", te.RawToolName)
	}
	if te.ActionType != "task_complete" {
		t.Errorf("ActionType = %q, want task_complete", te.ActionType)
	}
	if !strings.Contains(te.ToolOutput, "analyze the search implementation") {
		t.Errorf("ToolOutput = %q, want assistant text body", te.ToolOutput)
	}
	wantSEID := "antigravity-struct-payload:uuid-at:step:0:assistant"
	if te.SourceEventID != wantSEID {
		t.Errorf("SourceEventID = %q, want %q", te.SourceEventID, wantSEID)
	}
}

// buildAssistantTextStep builds a 1.2[] step entry with timestamp +
// 1.2.20.1 assistant response body. Matches the FB48 wire shape
// verified 2026-05-04 (1.2.20 = PLANNER_RESPONSE, 1.2.20.1 carries
// the actual model output text).
func buildAssistantTextStep(t *testing.T, ts uint64, body string) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil, 5,
		protowire.AppendBytesField(
			nil, 1,
			protowire.AppendVarintField(nil, 1, ts),
		),
	)
	envelope := protowire.AppendBytesField(nil, 1, []byte(body))
	stepBody = append(stepBody, protowire.AppendBytesField(nil, 20, envelope)...)
	return protowire.AppendBytesField(nil, 2, stepBody)
}

// buildUserPromptStep builds a 1.2[] step entry with timestamp +
// 1.2.19.2 user prompt body. Matches the FB48 wire shape verified
// 2026-05-04 (1.2.19 carries the user-message-with-IDE-context
// envelope; 1.2.19.2 is the prompt text itself).
func buildUserPromptStep(t *testing.T, ts uint64, prompt string) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil, 5,
		protowire.AppendBytesField(
			nil, 1,
			protowire.AppendVarintField(nil, 1, ts),
		),
	)
	envelope := protowire.AppendBytesField(nil, 2, []byte(prompt))
	stepBody = append(stepBody, protowire.AppendBytesField(nil, 19, envelope)...)
	return protowire.AppendBytesField(nil, 2, stepBody)
}

// TestParseStructuredTrajectory_PlanStepExtraction (Tier 5) verifies
// that 1.2.93 plan-step content (enum 1.2.1 = 81) is emitted as a
// structured.plan_step ToolEvent. Step description and analysis live
// at 1.2.93.2 / .3; status varint at 1.2.93.5.
func TestParseStructuredTrajectory_PlanStepExtraction(t *testing.T) {
	turn1 := protowire.AppendBytesField(nil, 3, buildTurn(t, 100, 0, 50, 1000, 0))
	step := buildPlanStep(
		t,
		1769687860,
		"Creating implementation plan for enhanced search algorithm",
		"Analyzing the current regulation search implementation to identify weaknesses and design improvements.",
		2,
	)
	wire := protowire.AppendBytesField(nil, 1, append(step, turn1...))

	got := ParseStructuredTrajectory(wire, "uuid-plan", "/tmp/proj", "/tmp/uuid-plan.pb", nil)

	if len(got.ToolEvents) != 1 {
		t.Fatalf("ToolEvents count = %d, want 1", len(got.ToolEvents))
	}
	te := got.ToolEvents[0]
	if te.RawToolName != "structured.plan_step" {
		t.Errorf("RawToolName = %q, want structured.plan_step", te.RawToolName)
	}
	if !strings.Contains(te.Target, "Creating implementation plan") {
		t.Errorf("Target = %q, want to contain step description", te.Target)
	}
	if !strings.Contains(te.ToolOutput, "Analyzing the current regulation") {
		t.Errorf("ToolOutput = %q, want analysis body", te.ToolOutput)
	}
	if te.RawToolInput != "2" {
		t.Errorf("RawToolInput = %q, want \"2\" (status enum)", te.RawToolInput)
	}
	wantSEID := "antigravity-struct-plan:uuid-plan:step:0"
	if te.SourceEventID != wantSEID {
		t.Errorf("SourceEventID = %q, want %q", te.SourceEventID, wantSEID)
	}
}

// TestParseStructuredTrajectory_FinalSummaryExtraction (Tier 6)
// verifies that 1.2.94 final-summary content (enum 1.2.1 = 82) is
// emitted as a structured.final_summary ToolEvent. URI at .1, body
// at .2.
func TestParseStructuredTrajectory_FinalSummaryExtraction(t *testing.T) {
	turn1 := protowire.AppendBytesField(nil, 3, buildTurn(t, 100, 0, 50, 1000, 0))
	step := buildFinalSummaryStep(
		t,
		1769687860,
		"file:///c:/programsx/regulation/INDUSTRY_MAPPINGS_REFERENCE.md",
		"✅ **Industry mappings are now comprehensive!** Expanded from 10 to 60+ industries.",
	)
	wire := protowire.AppendBytesField(nil, 1, append(step, turn1...))

	got := ParseStructuredTrajectory(wire, "uuid-final", "/tmp/proj", "/tmp/uuid-final.pb", nil)

	if len(got.ToolEvents) != 1 {
		t.Fatalf("ToolEvents count = %d, want 1", len(got.ToolEvents))
	}
	te := got.ToolEvents[0]
	if te.RawToolName != "structured.final_summary" {
		t.Errorf("RawToolName = %q, want structured.final_summary", te.RawToolName)
	}
	if !strings.Contains(te.ToolOutput, "Industry mappings are now comprehensive") {
		t.Errorf("ToolOutput = %q, want summary body", te.ToolOutput)
	}
	wantURI := "c:/programsx/regulation/INDUSTRY_MAPPINGS_REFERENCE.md"
	if te.RawToolInput != wantURI {
		t.Errorf("RawToolInput = %q, want %q (decoded URI)", te.RawToolInput, wantURI)
	}
	wantSEID := "antigravity-struct-final:uuid-final:step:0"
	if te.SourceEventID != wantSEID {
		t.Errorf("SourceEventID = %q, want %q", te.SourceEventID, wantSEID)
	}
}

// buildPlanStep builds a 1.2[] step entry with 1.2.93.x plan-step
// content (FB48 wire shape verified 2026-05-04: enum 1.2.1 = 81 hosts
// title at .1, step description at .2, long analysis at .3, code-link
// annotated copy at .4, status varint at .5).
func buildPlanStep(t *testing.T, ts uint64, desc, analysis string, status uint64) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil, 5,
		protowire.AppendBytesField(
			nil, 1,
			protowire.AppendVarintField(nil, 1, ts),
		),
	)
	envelope := protowire.AppendBytesField(nil, 2, []byte(desc))
	envelope = append(envelope, protowire.AppendBytesField(nil, 3, []byte(analysis))...)
	envelope = append(envelope, protowire.AppendVarintField(nil, 5, status)...)
	stepBody = append(stepBody, protowire.AppendBytesField(nil, 93, envelope)...)
	return protowire.AppendBytesField(nil, 2, stepBody)
}

// buildFinalSummaryStep builds a 1.2[] step entry with 1.2.94.x
// final-summary content (FB48 wire shape verified 2026-05-04: enum
// 1.2.1 = 82 hosts referenced URI at .1, summary body at .2).
func buildFinalSummaryStep(t *testing.T, ts uint64, uri, body string) []byte {
	t.Helper()
	stepBody := protowire.AppendBytesField(
		nil, 5,
		protowire.AppendBytesField(
			nil, 1,
			protowire.AppendVarintField(nil, 1, ts),
		),
	)
	envelope := protowire.AppendBytesField(nil, 1, []byte(uri))
	envelope = append(envelope, protowire.AppendBytesField(nil, 2, []byte(body))...)
	stepBody = append(stepBody, protowire.AppendBytesField(nil, 94, envelope)...)
	return protowire.AppendBytesField(nil, 2, stepBody)
}

// TestParseStructuredTrajectory_ArtifactBrainFallback (Tier 1)
// verifies that when a Flavor A edit lacks the workspace dir +
// basename pair (only the brain artifact URI), the parser falls
// back to the brain URI for the target — matching the "edit
// task.md" pattern observed twice in FB48.
func TestParseStructuredTrajectory_ArtifactBrainFallback(t *testing.T) {
	turn1 := protowire.AppendBytesField(nil, 3, buildTurn(t, 100, 0, 50, 1000, 0))
	step := buildArtifactStepFlavorA(
		t,
		1769687860,
		"Update task progress",
		"file:///C:/Users/u/.gemini/antigravity/brain/uuid/task.md",
		"", // workspace dir empty
		"", // basename empty
		"",
		"## Task list updates",
	)
	wire := protowire.AppendBytesField(nil, 1, append(step, turn1...))
	got := ParseStructuredTrajectory(wire, "uuid-art", "/tmp/proj", "/tmp/uuid-art.pb", nil)
	if len(got.ToolEvents) != 1 {
		t.Fatalf("ToolEvents count = %d, want 1", len(got.ToolEvents))
	}
	wantTarget := "C:/Users/u/.gemini/antigravity/brain/uuid/task.md"
	if got.ToolEvents[0].Target != wantTarget {
		t.Errorf("Target = %q, want %q (brain URI fallback when no workspace pair)", got.ToolEvents[0].Target, wantTarget)
	}
}
