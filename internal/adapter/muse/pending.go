package muse

import (
	"os"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// A Muse tool call and its result are FOUR separate records —
// assistant_tool_calls_committed, tool_batch.effect.started,
// tool_batch.effect.terminal (the success verdict) and
// tool_result_batch_committed (the body) — written however many seconds (or
// minutes) apart the tool took to run. A poll tick that ends between them
// would persist the call optimistically successful and then DISCARD both the
// verdict and the body: the call_id → ToolEvent correlation is parse-local,
// so the next parse resumes past the call record with an empty map and drops
// the later records on the floor. The premature Success=true is PERMANENT —
// store's action `ON CONFLICT(source_file, source_event_id)` clause updates
// duration_ms / metadata / raw_tool_input / raw_tool_output / content_bytes /
// an unknown action_type, and NOTHING else. Neither `success` nor
// `error_message` can be flipped by re-emitting the same SourceEventID, so
// the group MUST be resolved inside one parse window.
//
// The fix keeps them in one window: at EOF, a tool call still waiting for its
// result batch rewinds the byte cursor to the start of its own record and
// drops every event from that record onward — structurally the same deferral
// the line loop already applies to a partially written trailing line. The
// next poll re-reads the record whole and, once the result has landed, emits
// ONE row carrying the real success / error / output.
//
// Two bounds stop the deferral from ever stalling ingestion, because a call
// whose result never arrives (interrupted session, crashed host) is not
// distinguishable from one still running:
//
//   - maxDeferTailBytes — the deferral point must be within this many bytes
//     of the parsed end. A log that has written well past an unanswered call
//     is plainly not waiting on it, so the call is flushed with the
//     optimistic outcome and the parse moves on. Muse interleaves a lot of
//     scheduler bookkeeping between a call and its result (16 records in the
//     Phase-0 capture's first bash call), so the window is generous.
//   - pendingResultGrace — the log must have been modified within this
//     window. An abandoned log stops growing, so its tail is flushed once
//     the grace expires. A long shell command writes nothing to the log
//     until it finishes, so mtime staleness during a live call is expected;
//     the window is deliberately wider than any plausible tool timeout.
const (
	maxDeferTailBytes  = 1 << 20
	pendingResultGrace = 90 * time.Minute
)

// pendingMark locates a tool call still awaiting its result batch: the
// rewind coordinates of the record that produced its ToolEvent (byte offset
// of the line, and the event-slice lengths from before the record was
// handled, so truncation removes the WHOLE record's output — every sibling
// tool call of the same batch included).
type pendingMark struct {
	lineStart int64
	toolLen   int
	tokenLen  int
}

// deferUnpairedTail rewinds res to just before the earliest tool call still
// awaiting its result batch, so the next parse sees the call and its result
// together. A no-op when nothing is pending or when either bound above says
// the result is not coming.
func (st *parseState) deferUnpairedTail(res *adapter.ParseResult) {
	mark, ok := st.earliestPending()
	if !ok {
		return
	}
	if res.NewOffset-mark.lineStart > maxDeferTailBytes {
		return
	}
	info, err := os.Stat(st.path)
	if err != nil || time.Since(info.ModTime()) > pendingResultGrace {
		return
	}
	if mark.toolLen <= len(res.ToolEvents) {
		res.ToolEvents = res.ToolEvents[:mark.toolLen]
	}
	if mark.tokenLen <= len(res.TokenEvents) {
		res.TokenEvents = res.TokenEvents[:mark.tokenLen]
	}
	res.NewOffset = mark.lineStart
	// The cursor did not advance, so ask the watcher to keep polling —
	// without this a brand-new log whose FIRST parse defers would get no
	// parse_cursors row at all and would only be rediscovered by the
	// periodic full scan.
	res.RetrySuggested = true
}

// earliestPending returns the pending call whose record starts earliest in
// the file.
func (st *parseState) earliestPending() (pendingMark, bool) {
	var (
		best  pendingMark
		found bool
	)
	for _, m := range st.pendingCall {
		if !found || m.lineStart < best.lineStart {
			best, found = m, true
		}
	}
	return best, found
}
