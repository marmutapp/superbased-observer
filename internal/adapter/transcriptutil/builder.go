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
	// pendingID stamps the NEXT message the builder creates (the next
	// user message, or the FIRST record of the next assistant exchange).
	// Set per-record via SetNextID; consumed (and cleared) at creation.
	// First-wins for a merged assistant exchange: only the exchange-open
	// consumes it, so the second constituent record's id never overwrites
	// the opener's. Formats with no per-record id never call SetNextID and
	// every message keeps ID "".
	pendingID string

	// textCap/inputCap/resultCap bound message text, tool-call input, and
	// tool-call result excerpts. New seeds them to the package defaults
	// (TextCap/InputCap/ResultCap — the P1 excerpt behavior). NewWithCaps
	// overrides them; a cap <= 0 means UNCAPPED (the full-cache handoff
	// carries un-excerpted read bodies — docs/session-handoff.md).
	textCap, inputCap, resultCap int
}

// New returns an empty Builder with the default excerpt caps.
func New() *Builder {
	return NewWithCaps(TextCap, InputCap, ResultCap)
}

// NewWithCaps returns an empty Builder with explicit excerpt caps. A cap
// <= 0 disables capping for that field, so the builder emits the source's
// full bytes — the `full_cache` carry mode reads with (0,0,0) to recover
// the actual read content the ResultCap excerpt otherwise hides.
func NewWithCaps(textCap, inputCap, resultCap int) *Builder {
	return &Builder{
		pending:   map[string]*models.ToolCallRef{},
		textCap:   textCap,
		inputCap:  inputCap,
		resultCap: resultCap,
	}
}

// SetNextID stamps id on the next message the builder creates — the next
// user message, or the FIRST record of the next assistant exchange (a
// merged multi-record exchange keeps its opening record's id; first-wins).
// An empty id is ignored (leaves any prior stamp intact is NOT the intent:
// callers stamp per-record, so an empty id clears the stamp so a message
// created before the next stamp carries no stale id). Adapters that have
// no per-record id never call this and every message carries "".
func (b *Builder) SetNextID(id string) {
	b.pendingID = id
}

// consumeID returns the pending id and clears it, so only the creating
// call takes it.
func (b *Builder) consumeID() string {
	id := b.pendingID
	b.pendingID = ""
	return id
}

// User closes any open assistant exchange and appends a user message.
// Empty/whitespace text is dropped.
func (b *Builder) User(text string, ts time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	b.Flush()
	m := models.TranscriptMessage{Role: models.TranscriptUser, Time: ts, ID: b.consumeID()}
	m.Text, m.Truncated = b.capFlag(text, b.textCap)
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
	b.cur.Text, b.cur.Truncated = b.capFlag(b.cur.Text+strings.TrimSpace(text), b.textCap)
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
		InputExcerpt: b.cap(input, b.inputCap),
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
	ref.ResultExcerpt = b.cap(result, b.resultCap)
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
		b.cur = &models.TranscriptMessage{Role: models.TranscriptAssistant, ID: b.consumeID()}
	}
	if model != "" {
		b.cur.Model = model
	}
	if !ts.IsZero() {
		b.cur.Time = ts
	}
}

// capFlag trims and caps s at the builder's cap n, reporting whether it
// was cut. n <= 0 means uncapped — the trimmed string passes through whole
// (the full_cache read path).
func (b *Builder) capFlag(s string, n int) (string, bool) {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s, false
	}
	return s[:n] + "…", true
}

// cap trims and caps s at the builder's cap n (n <= 0 = uncapped).
func (b *Builder) cap(s string, n int) string {
	out, _ := b.capFlag(s, n)
	return out
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
