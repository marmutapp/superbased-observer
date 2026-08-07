package primeagent

import (
	"os"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// A toolCall and its `toolResult` are two SEPARATE session entries,
// written however long the tool took apart. A poll tick that ends between
// them would persist the call optimistically successful and then DISCARD
// the outcome: the call → ToolEvent correlation is parse-local, so the
// next parse resumes past the call with an empty map and drops the result
// on the floor. The premature Success=true is PERMANENT — store's action
// `ON CONFLICT(source_file, source_event_id)` clause updates duration /
// metadata / raw input / raw output / content_bytes / an unknown
// action_type, and nothing else. Neither `success` nor `error_message`
// can be flipped by re-emitting the same SourceEventID, so the pair must
// resolve inside ONE parse window.
//
// The deferral keeps it there: at EOF, a call still awaiting its result
// rewinds the byte cursor to the start of its own entry and drops every
// event from that entry onward — structurally the same deferral the line
// loop already applies to a partially written trailing line. The next
// poll re-reads the entry whole and, once the result has landed, emits
// ONE row carrying the real success / error / output / duration.
//
// Two bounds stop the deferral from stalling ingestion, because a call
// whose result never arrives (interrupted session, crashed kernel) is not
// distinguishable from one still running:
//
//   - maxDeferTailBytes — the deferral point must be within this many
//     bytes of the parsed end. A transcript that has written well past an
//     unanswered call is plainly not waiting on it.
//   - pendingResultGrace — the file must have been modified within this
//     window. An abandoned transcript stops growing, so its tail is
//     flushed once the grace expires. The window is deliberately wide: a
//     long-running kernel cell writes nothing to the transcript until it
//     finishes, so mtime staleness during a live call is expected.
const (
	maxDeferTailBytes  = 1 << 20
	pendingResultGrace = 90 * time.Minute
)

// pendingMark locates a still-unanswered toolCall: the index of its
// ToolEvent, plus the rewind coordinates of the entry that produced it
// (the byte offset of its line, and the event-slice lengths from before
// the entry was handled, so truncation removes the WHOLE entry's output —
// its sibling assistant-message row and its usage-derived TokenEvent
// included).
type pendingMark struct {
	idx       int
	lineStart int64
	toolLen   int
	tokenLen  int
}

// deferUnpairedTail rewinds res to just before the earliest toolCall
// still awaiting its result, so the next parse sees call and result
// together. A no-op when nothing is pending or when either bound says the
// result is not coming.
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

// earliestPending returns the pending call whose entry starts earliest in
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
