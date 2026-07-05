package transcriptutil

import (
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Excerpt caps shared by every reader (the P1 claude-code/codex values).
const (
	// TextCap bounds a message's flattened text.
	TextCap = 4000
	// InputCap bounds a tool call's input excerpt.
	InputCap = 200
	// ResultCap bounds a tool call's result excerpt.
	ResultCap = 300
)

// Builder folds a source format's records into normalized transcript
// messages. Zero value is NOT ready — use New.
//
// Tool calls accumulate as individually heap-allocated refs and
// materialize into the exchange's ToolCalls slice only at Flush — a
// pointer into an append-grown []ToolCallRef goes stale on reallocation,
// silently losing resolves for multi-call exchanges (found by
// TestBuilder_ResolveAll; the P1 per-adapter builders carried the same
// latent aliasing).
type Builder struct {
	msgs     []models.TranscriptMessage
	cur      *models.TranscriptMessage
	curCalls []*models.ToolCallRef
	pending  map[string]*models.ToolCallRef
	// anon tracks calls appended without an id (formats that never
	// record results, e.g. cursor agent-transcripts) so ResolveAll can
	// settle them on turn boundaries.
	anon []*models.ToolCallRef
}

// New returns an empty Builder.
func New() *Builder {
	return &Builder{pending: map[string]*models.ToolCallRef{}}
}

// User closes any open assistant exchange and appends a user message.
// Empty/whitespace text is dropped.
func (b *Builder) User(text string, ts time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.Flush()
	m := models.TranscriptMessage{Role: models.TranscriptUser, Time: ts}
	m.Text, m.Truncated = CapFlag(text, TextCap)
	b.msgs = append(b.msgs, m)
}

// AssistantText appends narration to the open assistant exchange
// (opening one if needed). model/ts refresh the exchange's metadata —
// exchange time is the LAST constituent record's time.
func (b *Builder) AssistantText(text, model string, ts time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.ensureAssistant(model, ts)
	if b.cur.Text != "" {
		b.cur.Text += "\n"
	}
	b.cur.Text, b.cur.Truncated = CapFlag(b.cur.Text+strings.TrimSpace(text), TextCap)
}

// AssistantCall appends a tool invocation to the open assistant
// exchange. Calls with an id become resolvable via Resolve; id-less
// calls settle on the next ResolveAll (formats that never record
// results).
func (b *Builder) AssistantCall(id, name, input, model string, ts time.Time) {
	b.ensureAssistant(model, ts)
	ref := &models.ToolCallRef{
		ID:           id,
		Name:         name,
		InputExcerpt: Cap(input, InputCap),
	}
	b.curCalls = append(b.curCalls, ref)
	if id != "" {
		b.pending[id] = ref
	} else {
		b.anon = append(b.anon, ref)
	}
}

// Resolve marks the pending call id resolved with a result excerpt.
// ts, when non-zero, refreshes the open exchange's completion time
// (codex semantics); pass the zero time to leave it (claude-code
// semantics — results ride user-role carriers with their own stamps).
func (b *Builder) Resolve(id, result string, ts time.Time) {
	ref, ok := b.pending[id]
	if !ok {
		return
	}
	ref.Resolved = true
	ref.ResultExcerpt = Cap(result, ResultCap)
	delete(b.pending, id)
	if b.cur != nil && !ts.IsZero() {
		b.cur.Time = ts
	}
}

// ResolveAll settles every id-less call appended so far — for formats
// whose files record a call only after it completed and never record
// results (cursor agent-transcripts: a turn_ended marker or the next
// user line proves the calls settled). Result excerpts stay empty:
// nothing was recorded, nothing is fabricated.
func (b *Builder) ResolveAll() {
	for _, ref := range b.anon {
		ref.Resolved = true
	}
	b.anon = nil
}

// Flush closes the open assistant exchange (dropping it when empty),
// materializing the accumulated call refs. Unresolved ids stop being
// resolvable — a result arriving after the exchange closed (i.e. after
// the user already spoke again) leaves its call honestly dangling.
func (b *Builder) Flush() {
	if b.cur == nil {
		return
	}
	for _, ref := range b.curCalls {
		b.cur.ToolCalls = append(b.cur.ToolCalls, *ref)
	}
	if b.cur.Text != "" || len(b.cur.ToolCalls) > 0 {
		b.msgs = append(b.msgs, *b.cur)
	}
	b.cur = nil
	b.curCalls = nil
	clear(b.pending)
	b.anon = nil
}

// Finish flushes and returns the normalized stream with indices set.
func (b *Builder) Finish() []models.TranscriptMessage {
	b.Flush()
	for i := range b.msgs {
		b.msgs[i].Index = i
	}
	return b.msgs
}

func (b *Builder) ensureAssistant(model string, ts time.Time) {
	if b.cur == nil {
		b.cur = &models.TranscriptMessage{Role: models.TranscriptAssistant}
	}
	if model != "" {
		b.cur.Model = model
	}
	if !ts.IsZero() {
		b.cur.Time = ts
	}
}

// CapFlag trims and caps a string, reporting whether it was cut.
func CapFlag(s string, n int) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s, false
	}
	return s[:n] + "…", true
}

// Cap trims and caps a string.
func Cap(s string, n int) string {
	out, _ := CapFlag(s, n)
	return out
}
