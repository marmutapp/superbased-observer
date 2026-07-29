package droid

import (
	"os"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// A tool_use and its tool_result are two SEPARATE JSONL records, written
// however many seconds (or minutes) apart the tool took to run. A poll
// tick that ends between them used to persist the call optimistically
// successful and then DISCARD the outcome entirely: the tool_use →
// ToolEvent correlation is parse-local, so the next parse resumed past
// the tool_use with an empty map and dropped the tool_result on the
// floor. The premature Success=true was permanent — store's action
// `ON CONFLICT(source_file, source_event_id)` clause updates
// duration_ms / metadata / raw_tool_input / raw_tool_output /
// content_bytes / an unknown action_type, and NOTHING else. Neither
// `success` nor `error_message` can be flipped by re-emitting the same
// SourceEventID, so the pair MUST be resolved inside one parse window.
//
// The fix keeps the pair in one window: at EOF, a tool_use still waiting
// for its result rewinds the byte cursor to the start of its own record
// and drops every event from that record onward — structurally the same
// deferral the line loop already applies to a partially written trailing
// line. The next poll re-reads the record whole and, once the result has
// landed, emits ONE row carrying the real success / error / output.
//
// Two bounds stop the deferral from ever stalling ingestion, because a
// tool_use whose result never arrives (interrupted session, crashed
// host) is not distinguishable from one still running:
//
//   - maxDeferTailBytes — the deferral point must be within this many
//     bytes of the parsed end. A transcript that has written well past
//     an unanswered call is plainly not waiting on it, so the call is
//     flushed with the optimistic outcome and the parse moves on.
//   - pendingResultGrace — the transcript must have been modified within
//     this window. An abandoned transcript stops growing, so its tail is
//     flushed once the grace expires. The window is deliberately wider
//     than the 30-minute ceiling claude-code documents for its longest
//     running tool: a long shell command writes nothing to the
//     transcript until it finishes, so mtime staleness during a live
//     call is expected.
const (
	maxDeferTailBytes  = 1 << 20
	pendingResultGrace = 90 * time.Minute
)

// pendingMark locates a still-unanswered tool_use: the index of its
// ToolEvent, plus the rewind coordinates of the record that produced it
// (byte offset of the line, and the event-slice lengths from before the
// record was handled, so truncation removes the WHOLE record's output,
// not just the tool row).
type pendingMark struct {
	idx       int
	lineStart int64
	toolLen   int
	tokenLen  int
}

// deferUnpairedTail rewinds res to just before the earliest tool_use
// still awaiting its tool_result, so the next parse sees the call and
// the result together. A no-op when nothing is pending or when either
// bound above says the result is not coming.
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
	// without this a brand-new transcript whose FIRST parse defers would
	// get no parse_cursors row at all and would only be rediscovered by
	// the periodic full scan.
	res.RetrySuggested = true
}

// earliestPending returns the pending tool_use whose record starts
// earliest in the file.
func (st *parseState) earliestPending() (pendingMark, bool) {
	var (
		best  pendingMark
		found bool
	)
	for _, m := range st.pendingTool {
		if !found || m.lineStart < best.lineStart {
			best, found = m, true
		}
	}
	return best, found
}
