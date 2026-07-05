package models

import "time"

// TranscriptRole classifies a normalized transcript message. Only real
// conversation messages carry a role: tool results are folded into the
// owning assistant message's ToolCalls (see ToolCallRef), never surfaced
// as messages of their own. This encodes the session-handoff Phase 0
// finding D-P0.3 — Anthropic-shaped formats deliver tool_result blocks
// inside user-role carrier records, which are NOT user messages, and a
// fork-point message index must count only real user/assistant messages.
type TranscriptRole string

const (
	// TranscriptUser is a real user prompt (typed by the operator or
	// injected as user-visible context), never a tool-result carrier.
	TranscriptUser TranscriptRole = "user"
	// TranscriptAssistant is one assistant exchange: all consecutive
	// assistant-side records (text, tool calls, their results) between two
	// user prompts, merged by the adapter's transcript reader.
	TranscriptAssistant TranscriptRole = "assistant"
)

// ToolCallRef is one tool invocation made within an assistant exchange,
// with excerpted input and result. Resolved is false while no result has
// been observed — the signal the handoff fork snap rule uses to refuse a
// cut inside an unresolved tool chain (plan §7).
type ToolCallRef struct {
	// ID is the source format's call id (tool_use.id, function_call.call_id).
	ID string
	// Name is the tool name as the source recorded it.
	Name string
	// InputExcerpt is a capped excerpt of the call input.
	InputExcerpt string
	// ResultExcerpt is a capped excerpt of the observed result, "" while
	// unresolved.
	ResultExcerpt string
	// Resolved reports whether the call's result was observed.
	Resolved bool
}

// TranscriptMessage is one normalized message of a session transcript,
// re-read from the source tool's OWN on-disk files at handoff time
// (docs/plans/session-handoff-plan-2026-07-03.md §6). Adapters produce
// this shape through their optional transcript-reader capability; nothing
// here is ever persisted to the observer DB — the DB stays content-free
// per the CLAUDE.md Don'ts.
type TranscriptMessage struct {
	// Index is the 0-based position in the normalized stream.
	Index int
	// Role is user or assistant (tool results fold into ToolCalls).
	Role TranscriptRole
	// Time anchors the message for as-of-fork filtering: the first
	// constituent record's time for user messages, the LAST constituent
	// record's time for assistant exchanges (completion time). Zero when
	// the source carries no per-message timestamps.
	Time time.Time
	// Text is the flattened natural-language content (thinking/reasoning
	// records are dropped at read time, per plan §8).
	Text string
	// ToolCalls lists the exchange's tool invocations in order.
	ToolCalls []ToolCallRef
	// Model is the per-message model id when the source records one.
	Model string
	// Truncated marks a message whose text was capped at read time.
	Truncated bool
}
