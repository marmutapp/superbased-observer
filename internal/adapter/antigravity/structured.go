package antigravity

import (
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// StructuredEnrichment is the per-conversation overlay extracted from
// GetCascadeTrajectory: model + real conversation start/end time,
// per-turn TokenEvents (one per LLM call), and per-step ToolEvents
// for the file-view tool calls observed in the trajectory's per-step
// stream. Layered on top of the markdown-recovery ParseResult so
// observer can show real token counts + model attribution + per-turn
// tool counts in the dashboard for sessions whose .pb files don't
// decrypt locally.
type StructuredEnrichment struct {
	Model       string
	StartedAt   time.Time
	EndedAt     time.Time
	TokenEvents []models.TokenEvent
	ToolEvents  []models.ToolEvent
}

// ParseStructuredTrajectory wire-walks the GetCascadeTrajectory
// response payload (5-byte gRPC frame header already stripped) and
// returns enrichment data. Exported so the backfill in cmd/observer
// can call it directly without an Adapter instance.
//
// Best-effort: missing or malformed fields produce zero values
// rather than errors. The markdown-recovery path has already
// handed the caller a usable ParseResult — any structured data on
// top is a bonus.
//
// Field-to-meaning mapping was verified against FB48 on 2026-05-04
// (see docs/handovers/antigravity-path-b-implementation-handoff-2026-05-04.md
// and docs/handovers/antigravity-tier-1-2-3-handoff-2026-05-04.md for the
// verification log):
//
//	1.3.3.28           → model name (e.g. "claude-sonnet-4-5")
//	1.3.1.17.2.1       → input_tokens (per-call non-cached prefix; const 333 on FB48)
//	1.3.1.17.2.2       → cache_creation_input_tokens (spikes on cache miss)
//	1.3.1.17.2.3       → total output_tokens (== .9 + .10; Gemini decomposes,
//	                     Claude has .9 absent so .10 == .3)
//	1.3.1.17.2.5       → cache_read_input_tokens (cumulative across turns)
//	1.3.1.17.2.9       → reasoning_output_tokens (Gemini-only; absent on
//	                     Claude). Universal invariant verified 2026-05-15
//	                     against 5 sessions / 33 turns covering Gemini Pro
//	                     low/high, Gemini Flash, and Claude Sonnet 4.6:
//	                     .3 == .9 + .10 holds on every turn. Mapped to
//	                     ReasoningTokens for separate cost attribution
//	                     (codex convention: reasoning + output additive).
//	1.3.1.17.2.10      → response_output_tokens (text-only output, excludes
//	                     reasoning). Mirror of .3 on Claude (no reasoning).
//	                     Mapped to OutputTokens so the cost engine bills
//	                     (.10 × output_rate + .9 × output_rate) = .3 × rate.
//	1.2.5.1.1          → per-step unix-seconds ts (min = session start, max = session end)
//	1.2.10.1.1.1       → artifact (Flavor A): task description
//	1.2.10.1.1.4.5     → artifact (Flavor A): brain/<uuid>/<artifact>.md URI
//	1.2.10.1.1.4.6.1   → artifact (Flavor A): workspace dir URI
//	1.2.10.1.1.4.6.2   → artifact (Flavor A): edited file basename
//	1.2.10.1.1.9.1     → artifact (Flavor A): before-edit body
//	1.2.10.1.1.9.2     → artifact (Flavor A): after-edit body
//	1.2.10.1.2.1       → artifact (Flavor B): full document body
//	1.2.10.1.2.2.5     → artifact (Flavor B): document URI
//	1.2.14.1           → file-view tool call: file URI
//	1.2.14.11          → file-view tool call: line count
//	1.2.14.12          → file-view tool call: byte size
//
// TokenEvent timestamps: distributed evenly across [StartedAt,
// EndedAt] so the dashboard's chronological view spans the actual
// conversation duration rather than collapsing to a 1-second-apart
// synthetic window.
//
// ToolEvent timestamps: actual per-step ts from 1.2.5.1.1 with
// duration_ms = next_step.ts - this_step.ts. Each tool gets
// MessageID matching the parent token row's MessageID via
// step-position-fraction (assigned_turn = step_idx * num_turns /
// num_steps), so the dashboard's per-message Tools count joins
// tools to their owning LLM call.
//
// scrubber is applied to every text field surfaced into a
// ToolEvent's Target / RawToolInput / PrecedingReasoning /
// ToolOutput. nil scrubber is treated as identity, but production
// callers MUST pass a real scrubber (per CLAUDE.md adapter rules).
//
// Step-type enum mapping (1.2.1) is not yet known beyond
// 90 = EPHEMERAL_MESSAGE, so per-message wall-clock alignment for
// markdown-derived rows + user-prompt extraction remain deferred —
// see the Tier 3 section of the handoff doc.
func ParseStructuredTrajectory(buf []byte, conversationID, projectRoot, sourceFile string, scrubber Scrubber) StructuredEnrichment {
	var en StructuredEnrichment
	if len(buf) == 0 || conversationID == "" {
		return en
	}
	if scrubber == nil {
		scrubber = identityScrubber{}
	}

	var rows []turnTokens
	var steps []stepData
	var minTimestamp, maxTimestamp uint64

	// Tier 4 — terminal-command snapshots embedded inside every
	// 1.2.19 user-message envelope (1.2.19.4.8.x). Each 1.2.19
	// carries a full snapshot of every live terminal session in the
	// IDE; the same command appears in every subsequent envelope, so
	// dedup by (terminalUUID, startSec) and keep the first sighting.
	var terminals []terminalRec
	var currentTerm *terminalRec
	flushTerminal := func() {
		if currentTerm != nil && (currentTerm.command != "" || currentTerm.uuid != "") {
			terminals = append(terminals, *currentTerm)
		}
		currentTerm = nil
	}

	_ = protowire.Walk(buf, func(f protowire.Field) error {
		switch {
		// Model name (string under the per-turn 1.3[].3 metadata wrapper).
		case pathEq(f.Path, 1, 3, 3, 28) && f.WireType == protowire.WireBytes:
			if en.Model == "" {
				en.Model = string(f.Bytes)
			}
		// New per-turn entry — push a row. Each 1.3[] is one LLM call.
		case pathEq(f.Path, 1, 3) && f.WireType == protowire.WireBytes:
			rows = append(rows, turnTokens{})
		// Token sub-fields under 1.3[].1 → .17 (usage) → .2 (counts).
		// .3 is the TOTAL output (sum of .9 + .10 on Gemini; equal to
		// .10 on Claude where .9 is absent). We capture .10 as the
		// response-only OutputTokens and .9 as ReasoningTokens so the
		// cost-engine's (output + reasoning) × output_rate math equals
		// .3 × output_rate (correct total, decomposed for visibility).
		// .3 itself is no longer mapped to OutputTokens — the codex
		// convention is reasoning additive-to-output, never subset.
		case len(f.Path) == 6 && pathPrefix(f.Path, 1, 3, 1, 17, 2) && f.WireType == protowire.WireVarint:
			if len(rows) == 0 {
				return nil
			}
			r := &rows[len(rows)-1]
			switch f.Path[5] {
			case 1:
				r.input = f.Varint
			case 2:
				r.cacheCreation = f.Varint
			case 5:
				r.cacheRead = f.Varint
			case 9:
				r.reasoning = f.Varint
			case 10:
				r.output = f.Varint
			}
		// New per-step entry — push a row. Each 1.2[] is one trajectory
		// event (user message / tool call / tool result / ephemeral
		// reminder / etc.; type enum at 1.2.1 not fully decoded).
		case pathEq(f.Path, 1, 2) && f.WireType == protowire.WireBytes:
			steps = append(steps, stepData{})
		// Per-step unix-seconds timestamp under 1.2[].5.1.1.
		case pathEq(f.Path, 1, 2, 5, 1, 1) && f.WireType == protowire.WireVarint:
			if len(steps) > 0 {
				steps[len(steps)-1].timestamp = f.Varint
			}
			if minTimestamp == 0 || f.Varint < minTimestamp {
				minTimestamp = f.Varint
			}
			if f.Varint > maxTimestamp {
				maxTimestamp = f.Varint
			}
		// File-view tool call: file URI (file:///c:/... or file:///home/...).
		case pathEq(f.Path, 1, 2, 14, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].fileURI = string(f.Bytes)
			}
		// File-view tool call: line count.
		case pathEq(f.Path, 1, 2, 14, 11) && f.WireType == protowire.WireVarint:
			if len(steps) > 0 {
				steps[len(steps)-1].fileLines = f.Varint
			}
		// File-view tool call: byte size.
		case pathEq(f.Path, 1, 2, 14, 12) && f.WireType == protowire.WireVarint:
			if len(steps) > 0 {
				steps[len(steps)-1].fileBytes = f.Varint
			}
		// Tier 1 — artifact edit/write events under 1.2.10.
		// Two flavors observed (each step carries one OR the other,
		// not both):
		//
		//   Flavor A — diff-style edit under 1.2.10.1.1.x: task
		//     description, brain artifact URI, optional
		//     workspace+basename pair when editing a workspace file,
		//     before/after body excerpts.
		//   Flavor B — full-document snapshot under 1.2.10.1.2.x:
		//     just the URI and the entire document body. Emitted on
		//     creation OR full overwrites (no diff to compute).
		case pathEq(f.Path, 1, 2, 10, 1, 1, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactDesc = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 1, 4, 5) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactBrainURI = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 1, 4, 6, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactWorkURI = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 1, 4, 6, 2) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactBasename = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 1, 9, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactBefore = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 1, 9, 2) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactAfter = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 2, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactDocBody = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 10, 1, 2, 2, 5) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].artifactDocURI = string(f.Bytes)
			}
		// Tier 2 — user message envelope at 1.2.19.2.
		//
		// 1.2.19 carries a "fresh user message" payload: the user
		// prompt text plus a snapshot of the IDE state (open files,
		// terminal history) at the moment the message was sent. On
		// FB48 it appears 3 times — exactly matching the 3 user
		// prompts in the markdown trajectory. The actual assistant
		// response text does NOT live inside 1.2.19; that's only
		// available via markdown's `### Planner Response`.
		case pathEq(f.Path, 1, 2, 19, 2) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].userPrompt = string(f.Bytes)
			}
		// Tier 4 — per-terminal-session command record under
		// 1.2.19.4.8. Walker emits parents and children, so the parent
		// 1.2.19.4.8 path acts as a record boundary: flush the
		// in-progress terminal and start a new accumulator.
		case pathEq(f.Path, 1, 2, 19, 4, 8) && f.WireType == protowire.WireBytes:
			flushTerminal()
			currentTerm = &terminalRec{stepIdx: len(steps) - 1}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 2) && f.WireType == protowire.WireBytes:
			if currentTerm != nil {
				currentTerm.command = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 3) && f.WireType == protowire.WireBytes:
			if currentTerm != nil {
				currentTerm.cwd = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 4) && f.WireType == protowire.WireBytes:
			if currentTerm != nil {
				currentTerm.output = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 6, 1) && f.WireType == protowire.WireVarint:
			if currentTerm != nil {
				currentTerm.startSec = f.Varint
			}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 7, 1) && f.WireType == protowire.WireVarint:
			if currentTerm != nil {
				currentTerm.exitCode = f.Varint
			}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 10) && f.WireType == protowire.WireBytes:
			if currentTerm != nil {
				currentTerm.uuid = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 19, 4, 8, 11, 1) && f.WireType == protowire.WireVarint:
			if currentTerm != nil {
				currentTerm.endSec = f.Varint
			}
		// Tier 3 — assistant response text at 1.2.20.1.
		//
		// 1.2.20 is the PLANNER_RESPONSE step (enum 1.2.1 = 15).
		// 1.2.20.1 carries the actual assistant text body. On FB48
		// this yields 29 records (vs. 26 LLM calls on the 1.3 axis,
		// since some turns produce multiple planner responses).
		// Emitting these as task_complete rows replaces the markdown
		// `### Planner Response` extraction with structured-truth
		// data anchored to real per-step timestamps.
		case pathEq(f.Path, 1, 2, 20, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].assistantText = string(f.Bytes)
			}
		// Tier 5 LEGACY — structured plan step at 1.2.93 (enum 1.2.1 = 81).
		// DEPRECATED 2026-05-13: zero hits on every modern session.
		// Retained for FB48 fixture + any pre-rewrite-era .pb files.
		// See `1.2.20.3` below for the current reasoning path.
		case pathEq(f.Path, 1, 2, 93, 2) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].planStepDesc = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 93, 3) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].planAnalysis = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 93, 5) && f.WireType == protowire.WireVarint:
			if len(steps) > 0 {
				steps[len(steps)-1].planStatus = f.Varint
			}
		// Tier 5 CURRENT — per-step reasoning body at 1.2.20.3.
		//
		// Companion to 1.2.20.1 (assistantText) on the same
		// PLANNER_RESPONSE step. Carries the model's internal
		// reasoning blob (e.g. "**Prioritizing Tool Specificity**\n\n
		// I'm focusing now on tool specificity..."). Verified
		// 2026-05-13 against 4 user sessions covering Claude Sonnet 4.6
		// + Gemini 3.1 Pro {low,high} + Gemini 3 Flash via the
		// path-inventory probe — fires 1-5 times per session
		// (planning_mode-on sessions get more hits).
		case pathEq(f.Path, 1, 2, 20, 3) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].reasoningText = string(f.Bytes)
			}
		// Tier 6 LEGACY — final summary at 1.2.94 (enum 1.2.1 = 82).
		// DEPRECATED 2026-05-13: zero hits on every modern session.
		// Retained for legacy fixtures. See `1.2.30.{4,5,15}` below.
		case pathEq(f.Path, 1, 2, 94, 1) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].finalSummaryURI = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 94, 2) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].finalSummary = string(f.Bytes)
			}
		// Tier 6 CURRENT — final summary envelope at 1.2.30.
		//
		// Fires 2× per session: once for the user-request summary,
		// once for the agent-response summary. Sub-fields:
		//   .4  = title (short, action-oriented header)
		//   .5  = body (formatted markdown summary)
		//   .15 = optional URI reference (e.g. brain artifact path)
		// Verified 2026-05-13 against 4 sessions — present on all,
		// regardless of model.
		case pathEq(f.Path, 1, 2, 30, 4) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].finalSummaryTitle = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 30, 5) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].finalSummary = string(f.Bytes)
			}
		case pathEq(f.Path, 1, 2, 30, 15) && f.WireType == protowire.WireBytes:
			if len(steps) > 0 {
				steps[len(steps)-1].finalSummaryURI = string(f.Bytes)
			}
		}
		return nil
	})
	flushTerminal()

	if minTimestamp > 0 {
		en.StartedAt = time.Unix(int64(minTimestamp), 0).UTC()
	}
	if maxTimestamp > 0 {
		en.EndedAt = time.Unix(int64(maxTimestamp), 0).UTC()
	}

	// Build TokenEvents — one per turn that reports usage. Indices are
	// preserved from rows[] so the i-th TokenEvent corresponds to the
	// i-th non-empty turn (used by step-to-turn assignment below).
	type turnSlot struct {
		emittedIdx int
		usable     bool
	}
	turnSlots := make([]turnSlot, len(rows))
	en.TokenEvents = make([]models.TokenEvent, 0, len(rows))
	for i, r := range rows {
		// Skip turns with no usage populated (placeholder rows for LLM
		// calls that errored before reporting tokens — see 1.3.1.17.3).
		if r.input == 0 && r.output == 0 && r.cacheRead == 0 && r.cacheCreation == 0 && r.reasoning == 0 {
			continue
		}
		turnSlots[i].emittedIdx = len(en.TokenEvents)
		turnSlots[i].usable = true
		ts := spreadTimestamp(en.StartedAt, en.EndedAt, len(en.TokenEvents), -1) // patched below
		en.TokenEvents = append(en.TokenEvents, models.TokenEvent{
			SourceFile:          sourceFile,
			SourceEventID:       "antigravity-struct-token:" + conversationID + ":" + intStr(i),
			SessionID:           conversationID,
			ProjectRoot:         projectRoot,
			Timestamp:           ts,
			Tool:                models.ToolAntigravity,
			Model:               en.Model,
			InputTokens:         int64(r.input),
			OutputTokens:        int64(r.output),
			CacheReadTokens:     int64(r.cacheRead),
			CacheCreationTokens: int64(r.cacheCreation),
			ReasoningTokens:     int64(r.reasoning),
			Source:              models.TokenSourceJSONL,
			Reliability:         models.ReliabilityApproximate,
			MessageID:           sharedTurnMessageID(conversationID, i),
		})
	}
	// Patch token timestamps now that we know the final emitted count.
	nTokens := len(en.TokenEvents)
	for i := range en.TokenEvents {
		en.TokenEvents[i].Timestamp = spreadTimestamp(en.StartedAt, en.EndedAt, i, nTokens)
	}

	// Build ToolEvents from per-step entries that look like file-view
	// tool calls (1.2.14.x) OR artifact edits (1.2.10.x). Each tool's
	// parent turn is computed by chronological position — step i out
	// of N steps maps to turn floor(i * num_turns / N) on the LLM-call
	// axis, then mapped to the original turn index so the MessageID
	// lines up.
	numSteps := len(steps)
	if numSteps > 0 && len(rows) > 0 {
		for i, s := range steps {
			rawTurnIdx := (i * len(rows)) / numSteps
			if rawTurnIdx >= len(rows) {
				rawTurnIdx = len(rows) - 1
			}
			ts := en.StartedAt
			if s.timestamp > 0 {
				ts = time.Unix(int64(s.timestamp), 0).UTC()
			}
			// Per-step duration = wall-clock to the next step's
			// timestamp. Capped at 1h: Antigravity sessions can sit
			// idle for hours/days between user turns, and that idle
			// time isn't tool-execution time. Without the cap, dashboard
			// `tool_time` aggregations report multi-day values (observed
			// 2026-05-13: 87 run_command rows across the maintainer's
			// DB totaling 2,976h of fake "tool time").
			var durationMs int64
			if s.timestamp > 0 && i+1 < numSteps && steps[i+1].timestamp > s.timestamp {
				gapSec := steps[i+1].timestamp - s.timestamp
				if gapSec < 3600 {
					durationMs = int64(gapSec) * 1000
				}
			}
			msgID := sharedTurnMessageID(conversationID, rawTurnIdx)

			if s.fileURI != "" {
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:    sourceFile,
					SourceEventID: "antigravity-struct-tool:" + conversationID + ":step:" + intStr(i),
					SessionID:     conversationID,
					ProjectRoot:   projectRoot,
					Timestamp:     ts,
					Tool:          models.ToolAntigravity,
					Model:         en.Model,
					ActionType:    models.ActionReadFile,
					Target:        truncate(decodeFileURIToPath(s.fileURI), 200),
					Success:       true,
					DurationMs:    durationMs,
					RawToolName:   "structured.file_view",
					MessageID:     msgID,
				})
			}

			if s.hasArtifact() {
				target, rawName, beforeBody, afterBody := classifyArtifact(s)
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:         sourceFile,
					SourceEventID:      "antigravity-struct-artifact:" + conversationID + ":step:" + intStr(i),
					SessionID:          conversationID,
					ProjectRoot:        projectRoot,
					Timestamp:          ts,
					Tool:               models.ToolAntigravity,
					Model:              en.Model,
					ActionType:         models.ActionEditFile,
					Target:             truncate(target, 200),
					Success:            true,
					DurationMs:         durationMs,
					RawToolName:        rawName,
					RawToolInput:       scrubber.String(truncate(s.artifactDesc, 800)),
					PrecedingReasoning: scrubber.String(truncate(beforeBody, 800)),
					ToolOutput:         scrubber.String(truncate(afterBody, 4000)),
					MessageID:          msgID,
				})
			}

			if s.userPrompt != "" {
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:    sourceFile,
					SourceEventID: "antigravity-struct-payload:" + conversationID + ":step:" + intStr(i) + ":user",
					SessionID:     conversationID,
					ProjectRoot:   projectRoot,
					Timestamp:     ts,
					Tool:          models.ToolAntigravity,
					Model:         en.Model,
					ActionType:    models.ActionUserPrompt,
					Target:        truncate(scrubber.String(s.userPrompt), 200),
					Success:       true,
					DurationMs:    durationMs,
					RawToolName:   "structured.user_prompt",
					RawToolInput:  scrubber.String(truncate(s.userPrompt, 4000)),
					MessageID:     msgID,
				})
			}

			if s.assistantText != "" {
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:         sourceFile,
					SourceEventID:      "antigravity-struct-payload:" + conversationID + ":step:" + intStr(i) + ":assistant",
					SessionID:          conversationID,
					ProjectRoot:        projectRoot,
					Timestamp:          ts,
					Tool:               models.ToolAntigravity,
					Model:              en.Model,
					ActionType:         models.ActionTaskComplete,
					Target:             truncate(scrubber.String(s.assistantText), 200),
					Success:            true,
					DurationMs:         durationMs,
					RawToolName:        "structured.assistant_text",
					PrecedingReasoning: truncate(scrubber.String(s.assistantText), 200),
					ToolOutput:         scrubber.String(truncate(s.assistantText, 4000)),
					MessageID:          msgID,
				})
			}

			// Legacy plan_step (1.2.93.x). Modern reasoning lives in
			// `reasoningText` (1.2.20.3) — emitted in the block below.
			if s.planStepDesc != "" || s.planAnalysis != "" {
				body := s.planAnalysis
				if body == "" {
					body = s.planStepDesc
				}
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:    sourceFile,
					SourceEventID: "antigravity-struct-plan:" + conversationID + ":step:" + intStr(i),
					SessionID:     conversationID,
					ProjectRoot:   projectRoot,
					Timestamp:     ts,
					Tool:          models.ToolAntigravity,
					Model:         en.Model,
					ActionType:    models.ActionTaskComplete,
					Target:        truncate(scrubber.String(s.planStepDesc), 200),
					Success:       true,
					DurationMs:    durationMs,
					RawToolName:   "structured.plan_step",
					RawToolInput:  intStr(int(s.planStatus)),
					ToolOutput:    scrubber.String(truncate(body, 4000)),
					MessageID:     msgID,
				})
			}

			// Modern reasoning (1.2.20.3). Sibling of assistantText on
			// the same PLANNER_RESPONSE step; surfaces the model's
			// internal analysis blob that's otherwise invisible in
			// markdown trajectory + the assistant-text emission.
			if s.reasoningText != "" {
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:    sourceFile,
					SourceEventID: "antigravity-struct-reasoning:" + conversationID + ":step:" + intStr(i),
					SessionID:     conversationID,
					ProjectRoot:   projectRoot,
					Timestamp:     ts,
					Tool:          models.ToolAntigravity,
					Model:         en.Model,
					ActionType:    models.ActionTaskComplete,
					Target:        truncate(scrubber.String(s.reasoningText), 200),
					Success:       true,
					DurationMs:    durationMs,
					RawToolName:   "structured.reasoning",
					ToolOutput:    scrubber.String(truncate(s.reasoningText, 4000)),
					MessageID:     msgID,
				})
			}

			if s.finalSummary != "" {
				// Prefer the title from 1.2.30.4 when present (modern
				// schema); fall back to the truncated body if not.
				target := s.finalSummaryTitle
				if target == "" {
					target = s.finalSummary
				}
				en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
					SourceFile:    sourceFile,
					SourceEventID: "antigravity-struct-final:" + conversationID + ":step:" + intStr(i),
					SessionID:     conversationID,
					ProjectRoot:   projectRoot,
					Timestamp:     ts,
					Tool:          models.ToolAntigravity,
					Model:         en.Model,
					ActionType:    models.ActionTaskComplete,
					Target:        truncate(scrubber.String(target), 200),
					Success:       true,
					DurationMs:    durationMs,
					RawToolName:   "structured.final_summary",
					RawToolInput:  scrubber.String(truncate(decodeFileURIToPath(s.finalSummaryURI), 200)),
					ToolOutput:    scrubber.String(truncate(s.finalSummary, 4000)),
					MessageID:     msgID,
				})
			}
		}
	}

	// Tier 4 — emit run_command ToolEvents from the terminal-history
	// snapshots embedded in 1.2.19. Each command appears in every
	// 1.2.19 envelope after it ran, so dedup by (uuid, startSec) and
	// keep the first (earliest) sighting. The MessageID joins the
	// command to whichever LLM-call turn was nearest in time to the
	// command's start — Tier 4's snapshots cluster on the FIRST step
	// where the terminal first appeared (often turn 0), but the
	// command itself may have run hours later, so the step-fraction
	// fallback used by other emit loops places every command on the
	// wrong turn. Token timestamps are spread evenly across
	// [StartedAt, EndedAt], which is close enough for nearest-turn
	// alignment to land in the right ballpark.
	if numSteps > 0 && len(rows) > 0 && len(terminals) > 0 {
		seen := map[string]bool{}
		for _, t := range terminals {
			if t.uuid == "" && t.startSec == 0 && t.command == "" {
				continue
			}
			key := t.uuid + ":" + intStr(int(t.startSec))
			if seen[key] {
				continue
			}
			seen[key] = true
			ts := en.StartedAt
			if t.startSec > 0 {
				ts = time.Unix(int64(t.startSec), 0).UTC()
			}
			// Terminal lifetime → tool-call duration is only meaningful
			// when the terminal was spawned during the AI session.
			// Pre-existing background terminals (long-running `codex`
			// REPLs, `claude --resume` shells, dev servers) have their
			// process lifetime captured here and would otherwise inflate
			// the dashboard's `tool_time` aggregation by hours — observed
			// 2026-05-13 in session 162c4ab9 where a 32h-old codex
			// terminal turned into a 32h "tool time" value.
			//
			// Guard: emit duration only when (a) the terminal started
			// after the session itself started, and (b) the resulting
			// duration is under 1h (no AI tool call legitimately takes
			// longer; if it ever does, the protobuf likely captured
			// process lifetime instead).
			var durationMs int64
			if t.endSec > t.startSec && t.startSec > 0 {
				sessionStartSec := uint64(0)
				if !en.StartedAt.IsZero() {
					sessionStartSec = uint64(en.StartedAt.Unix())
				}
				if (sessionStartSec == 0 || t.startSec >= sessionStartSec) &&
					t.endSec-t.startSec < 3600 {
					durationMs = int64(t.endSec-t.startSec) * 1000
				}
			}
			// Exit code is encoded as a signed varint via two's
			// complement on uint64. -2147483648 (the "no exit yet"
			// sentinel observed on FB48) shows up as 18446744011573954816.
			exitSigned := int64(t.exitCode)
			success := exitSigned == 0
			msgID := nearestTokenMessageID(ts, en.TokenEvents, conversationID)
			en.ToolEvents = append(en.ToolEvents, models.ToolEvent{
				SourceFile:    sourceFile,
				SourceEventID: "antigravity-struct-cmd:" + conversationID + ":term:" + t.uuid + ":" + intStr(int(t.startSec)),
				SessionID:     conversationID,
				ProjectRoot:   projectRoot,
				Timestamp:     ts,
				Tool:          models.ToolAntigravity,
				Model:         en.Model,
				ActionType:    models.ActionRunCommand,
				Target:        truncate(scrubber.String(t.command), 200),
				Success:       success,
				DurationMs:    durationMs,
				RawToolName:   "structured.run_command",
				RawToolInput:  scrubber.String(truncate(t.cwd, 200)),
				ToolOutput:    scrubber.String(truncate(t.output, 4000)),
				MessageID:     msgID,
			})
		}
	}
	return en
}

// terminalRec accumulates one terminal-session snapshot from
// 1.2.19.4.8.x during the wire walk. Multiple snapshots per
// 1.2.19 envelope (one per live terminal); same record repeats
// across envelopes. stepIdx records which 1.2[] step the snapshot
// arrived under so the emitted ToolEvent's MessageID joins to the
// right turn cluster.
type terminalRec struct {
	stepIdx  int
	uuid     string
	command  string
	cwd      string
	output   string
	startSec uint64
	endSec   uint64
	exitCode uint64
}

// classifyArtifact picks the best display target + raw_tool_name for
// a 1.2.10 step.
//
// Flavor A (`structured.artifact_edit`) carries before/after diff
// bodies; the target prefers the workspace dir + basename pair when
// present (real workspace edit), then falls back to the brain
// artifact URI (e.g. brain/<uuid>/task.md, the agent's todo list).
//
// Flavor B (`structured.artifact_write`) carries a full-document
// snapshot with no diff; the target is the document URI directly.
// Flavor B's `afterBody` is the entire document so the dashboard's
// expand-row view shows the file's state at the time of the write.
func classifyArtifact(s stepData) (target, rawName, beforeBody, afterBody string) {
	switch {
	case s.artifactBasename != "" || s.artifactBefore != "" || s.artifactAfter != "":
		// Flavor A: diff-style edit.
		switch {
		case s.artifactBasename != "" && s.artifactWorkURI != "":
			target = decodeFileURIToPath(s.artifactWorkURI) + "/" + s.artifactBasename
		case s.artifactBrainURI != "":
			target = decodeFileURIToPath(s.artifactBrainURI)
		case s.artifactBasename != "":
			target = s.artifactBasename
		}
		return target, "structured.artifact_edit", s.artifactBefore, s.artifactAfter
	default:
		// Flavor B: full-document snapshot.
		target = decodeFileURIToPath(s.artifactDocURI)
		return target, "structured.artifact_write", "", s.artifactDocBody
	}
}

// identityScrubber is the no-op scrubber used when callers pass nil.
// Adapter callers always pass a real *scrub.Scrubber; tests may pass
// nil for simplicity.
type identityScrubber struct{}

// String returns the input unchanged.
func (identityScrubber) String(s string) string { return s }

// turnTokens accumulates per-turn token counts as the walker fires.
// output is response-only (path .10); reasoning is the thinking-portion
// (path .9, Gemini-only). Codex convention applies: cost = (output +
// reasoning) × output_rate.
type turnTokens struct {
	input, output, cacheRead, cacheCreation, reasoning uint64
}

// stepData accumulates per-step (1.2[]) sub-fields the parser cares
// about. Other step types (1.2.103 ephemeral reminders, 1.2.111
// cross-conversation context, 1.2.19 large assistant payloads —
// Tier 2 of the deep-extraction handoff) aren't materialized here.
type stepData struct {
	timestamp uint64
	// 1.2.14.x — file-view tool call.
	fileURI   string
	fileLines uint64
	fileBytes uint64
	// 1.2.10.1.1.x — artifact diff-style edit (Flavor A).
	artifactDesc     string
	artifactBrainURI string
	artifactWorkURI  string
	artifactBasename string
	artifactBefore   string
	artifactAfter    string
	// 1.2.10.1.2.x — artifact full-doc snapshot (Flavor B).
	artifactDocBody string
	artifactDocURI  string
	// 1.2.19.2 — Tier 2 user prompt text (only set on steps that
	// represent a fresh user message, not on tool-result follow-ups).
	userPrompt string
	// 1.2.20.1 — Tier 3 assistant response text from the
	// PLANNER_RESPONSE step (enum 1.2.1 = 15).
	assistantText string
	// 1.2.93.x — Tier 5 plan-step content (enum 1.2.1 = 81).
	// Step description is action-oriented ("Creating implementation
	// plan…"); analysis is the long-form explanation; status is a
	// small enum (observed: 1, 2, 3).
	//
	// DEPRECATED 2026-05-13: zero hits on any modern Antigravity
	// session (Claude-Sonnet, Gemini-3.1-Pro-{low,high}, Gemini-3-Flash
	// — all verified via path inventory). Cases retained as legacy
	// fallback for the FB48 fixture + any old sessions that still use
	// the pre-rewrite schema. Modern reasoning text lives at
	// `1.2.20.3` (see reasoningText below).
	planStepDesc string
	planAnalysis string
	planStatus   uint64
	// 1.2.20.3 — per-step reasoning text. Emitted by Gemini agent
	// modes when planning_mode is on (`1.2.103.3 = "planning_mode"`);
	// Claude sessions populate it less often. Verified 2026-05-13
	// against 4 user sessions: counts 5 / 1 / 5 / 4 per session for
	// Gemini-Pro-high / Sonnet-4-6 / Gemini-Pro-low / Gemini-Flash
	// respectively. Surfaces as `structured.reasoning` ToolEvents.
	reasoningText string
	// 1.2.94.x — Tier 6 final-summary content (enum 1.2.1 = 82).
	// DEPRECATED 2026-05-13: see planStepDesc note above. Modern
	// final summary lives at `1.2.30.{4,5,15}`.
	//
	// finalSummary is the formatted markdown sign-off text;
	// finalSummaryURI references a file the summary calls out.
	finalSummary    string
	finalSummaryURI string
	// 1.2.30.4 — title for the final-summary envelope. Companion to
	// finalSummary (1.2.30.5). Fires 2× per session in current
	// Antigravity (verified 2026-05-13): once for the "user-request
	// summary" pair, once for the "agent-response summary" pair.
	finalSummaryTitle string
}

// hasArtifact returns true when this step carries 1.2.10 artifact
// data (either Flavor A or Flavor B).
func (s stepData) hasArtifact() bool {
	return s.artifactBasename != "" ||
		s.artifactBrainURI != "" ||
		s.artifactDocURI != "" ||
		s.artifactDocBody != ""
}

// sharedTurnMessageID is the MessageID scheme used by both
// TokenEvents and ToolEvents emitted from the structured trajectory.
// The dashboard groups action rows under their owning token row by
// matching message_id, so token + tool emissions for the same turn
// must produce the same value here. The "antigravity:" prefix
// distinguishes structured-derived ids from the markdown extractor's
// "antigravity-md:" scheme (which keys off markdown's user/assistant
// turn counter — a different axis that doesn't align numerically
// with structured 1.3[] indices).
func sharedTurnMessageID(conversationID string, turnIdx int) string {
	return "antigravity:" + conversationID + ":turn:" + intStr(turnIdx)
}

// nearestTokenMessageID returns the MessageID of the TokenEvent
// whose Timestamp is closest to ts. Used for emissions that don't
// have a meaningful step index in the trajectory's 1.2[] axis (e.g.
// run_command snapshots, which appear on the FIRST step where a
// terminal first showed up but actually executed at t.startSec —
// often hours later for long-lived `npm run dev` style processes).
//
// Falls back to turn 0 when tokens is empty.
func nearestTokenMessageID(ts time.Time, tokens []models.TokenEvent, conversationID string) string {
	if len(tokens) == 0 {
		return sharedTurnMessageID(conversationID, 0)
	}
	bestIdx := 0
	bestDiff := absDuration(tokens[0].Timestamp.Sub(ts))
	for i := 1; i < len(tokens); i++ {
		d := absDuration(tokens[i].Timestamp.Sub(ts))
		if d < bestDiff {
			bestDiff = d
			bestIdx = i
		}
	}
	return tokens[bestIdx].MessageID
}

// absDuration returns the absolute value of a duration.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// spreadTimestamp distributes the i-th of n events evenly across
// [start, end]. Falls back to start + i*1s when the window is
// degenerate (zero duration, single event, or unknown end). Used so
// 26 token events across a 30-minute conversation actually look like
// they happened over 30 minutes, not 26 seconds.
func spreadTimestamp(start, end time.Time, i, n int) time.Time {
	if start.IsZero() {
		// Preserve the synthetic monotonic ordering even with no real
		// anchor — the caller wants stable per-turn timestamps for
		// dashboard sort.
		return time.Unix(int64(i), 0).UTC()
	}
	if end.IsZero() || !end.After(start) || n <= 1 {
		return start.Add(time.Duration(i) * time.Second)
	}
	dur := end.Sub(start)
	offset := time.Duration(int64(dur) * int64(i) / int64(n-1))
	return start.Add(offset)
}

// decodeFileURIToPath converts a file:/// URI from a Windows-side
// trajectory into a DISPLAY-ready path — the shape the user sees in
// Antigravity's UI. Used for RawToolInput / Target fields rendered
// in the dashboard's tool-call detail view; intentionally does NOT
// apply cross-mount translation because the user expects the
// Windows-shaped path back (decodeFileURIToRoot is the
// translate-for-stat variant used by project-root resolution).
//
// v1.6.29 kept this function hand-rolled for that exact display-only
// contract; switching to pathnorm.Normalize would inject cross-mount
// translation that changes the displayed value (operator-rejected
// behaviour change — see TestParseStructuredTrajectory_*).
func decodeFileURIToPath(uri string) string {
	if uri == "" {
		return ""
	}
	// Strip file:/// prefix; tolerate file:// (no third slash) too.
	switch {
	case strings.HasPrefix(uri, "file:///"):
		uri = strings.TrimPrefix(uri, "file:///")
	case strings.HasPrefix(uri, "file://"):
		uri = strings.TrimPrefix(uri, "file://")
	}
	// Best-effort %-decode of the most common patterns; full
	// url.PathUnescape is overkill and brings stdlib import weight
	// for one path string.
	uri = strings.ReplaceAll(uri, "%3A", ":")
	uri = strings.ReplaceAll(uri, "%3a", ":")
	uri = strings.ReplaceAll(uri, "%20", " ")
	uri = strings.ReplaceAll(uri, "%2F", "/")
	uri = strings.ReplaceAll(uri, "%2f", "/")
	return uri
}

// pathEq returns true when path is exactly the supplied field-number
// sequence. Cheaper than slice comparison in the hot walk loop.
func pathEq(path []int, want ...int) bool {
	if len(path) != len(want) {
		return false
	}
	for i, w := range want {
		if path[i] != w {
			return false
		}
	}
	return true
}

// pathPrefix returns true when path starts with the supplied
// field-number prefix. The walker emits both parents and children, so
// callers use pathPrefix when the suffix carries the discriminator.
func pathPrefix(path []int, want ...int) bool {
	if len(path) < len(want) {
		return false
	}
	for i, w := range want {
		if path[i] != w {
			return false
		}
	}
	return true
}
