package browserchat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// CapturedTurnSchemaVersion is the current wire-contract version. The
// extension stamps SchemaVersion on every payload; a mismatch is tolerated
// (unknown-but-newer fields are ignored by encoding/json) but logged, so a
// version-skewed bridge is visible rather than silently lossy.
const CapturedTurnSchemaVersion = 1

// previewMaxBytes bounds the Target preview line stored on the action row.
const previewMaxBytes = 120

// CapturedTurn is the wire contract the browser extension → native host →
// `observer browser hook` all agree on. See doc.go for the field-by-field
// schema. Unknown fields are tolerated; missing fields surface as zero
// values.
type CapturedTurn struct {
	SchemaVersion     int    `json:"schema_version"`
	Site              string `json:"site"`
	ConversationID    string `json:"conversation_id"`
	MessageID         string `json:"message_id"`
	Model             string `json:"model"`
	RequestURL        string `json:"request_url"`
	PromptText        string `json:"prompt_text"`
	ResponseText      string `json:"response_text"`
	PromptTokensEst   int64  `json:"prompt_tokens_est"`
	ResponseTokensEst int64  `json:"response_tokens_est"`
	LatencyMs         int64  `json:"latency_ms"`
	CapturedAt        string `json:"captured_at"`
	Granularity       string `json:"granularity"`
	Title             string `json:"title"`
	// IDSource is the per-turn provenance of how the extension obtained
	// conversation_id: "none" | "request" | "stream" | "resume" | "url" |
	// "chain" ("url"/"chain" = the Perplexity thread resolver's /search/<id>
	// pathname read and last_backend_uuid chain map, added 2026-07-18).
	// "none" means no real id was recovered and a synthetic key is expected.
	// Additive/omitempty-tolerant: an older bridge omits it (zero value "").
	// The field is parsed here so it isn't silently dropped; the daemon routes
	// it to browser-health telemetry (an id_source_none counter) rather than a
	// DB column — see cmd/observer/browser.go peekBrowserTurn.
	IDSource string `json:"id_source"`
	// CaptureID is an opaque, random per-turn id the extension mints
	// (crypto.randomUUID) and stamps on EVERY emitted turn at every
	// granularity. It is IDENTICAL across a turn's tool event and token event
	// because both are built from the same payload. It is the id-less-turn
	// fallback session-key tier, sitting BELOW a real conversation_id /
	// message_id: unlike the old content fingerprint it groups the one turn's
	// two events WITHOUT deriving from — or embedding — any prompt/response
	// content, so a session key built from it never carries a content-derived
	// value across the org-push privacy boundary (MED-1 / MED-3). Additive: an
	// older pre-fix bridge omits it (zero value ""), in which case the
	// captured_at last-resort covers the turn.
	CaptureID string `json:"capture_id"`
}

// flexString is a string field that tolerates its JSON value arriving as
// EITHER a plain string OR an array of strings / objects carrying a `text`
// field. The browser extension contract sends the free-text content fields
// (prompt_text / response_text / title) as plain strings — parsers.js coerces
// them at the emission chokepoint — but a stale/older bridge, or a site whose
// wire echoes the user turn as an array of message parts / adaptive-card
// segments (consumer Copilot), can still ship an array. Failing the WHOLE turn
// on that (the pre-fix behavior: `json: cannot unmarshal array into Go struct
// field CapturedTurn.prompt_text of type string`) drops a capture we could
// have kept, so this decoder is defense-in-depth: it JOINS the real text parts
// and IGNORES junk, and NEVER errors — a value that is neither a string nor a
// text-bearing array degrades to "" and parsing of the rest of the turn
// continues (capture-something beats drop-everything). It does not invent
// content: only genuine string / `.text` parts are joined, non-text parts are
// dropped, and the delimiter between distinct parts is "\n".
type flexString string

// UnmarshalJSON decodes a string OR an array-of-parts into a plain string,
// degrading any other shape (number, bool, object, null) to "" WITHOUT
// returning an error. The bytes handed to a custom UnmarshalJSON are always a
// syntactically valid JSON value (the outer decoder already tokenized them);
// walkFlexBytes streams them through a bounded json.Decoder token walk so the
// traversal bounds bind before materialization (finding 1).
func (f *flexString) UnmarshalJSON(data []byte) error {
	acc := newCoerceAcc()
	walkFlexBytes(data, &acc)
	*f = flexString(strings.Join(acc.texts, "\n"))
	return nil
}

// Traversal bounds for the walkFlex* token walk. A captured content value can be a
// hostile payload (a stale/spoofed bridge, an adversarial site echo): a
// deeply-nested tree blows the stack/allocates disproportionately, a huge flat
// array pins the CPU, and a multi-MiB payload balloons memory. So the walk is
// bounded on FOUR axes — nesting depth, a whole-walk visit ceiling (CPU/alloc),
// per-slot fragment count, and per-slot output bytes — and on ANY bound hit it
// KEEPS what it already accumulated (truncate, never nuke to empty). These
// mirror the parsers.js coerceCollect bounds; the byte cap is
// contentcap.DefaultMaxBytes (the same cap Normalize later applies) so we never
// build more than survives downstream.
const (
	maxCoerceDepth = 8
	maxCoerceParts = 256
	// maxCoerceBytes is a small slack ABOVE contentcap.DefaultMaxBytes (the cap
	// Normalize later applies) so Normalize's contentcap.Cap still detects an
	// over-cap result and appends its legible truncation marker. The slack is
	// ≪ the cap, so an 8 MiB hostile payload is still bounded to ~1 MiB.
	maxCoerceBytes = contentcap.DefaultMaxBytes + 4096
	// maxCoerceVisits is the GLOBAL ceiling on VALUES touched across an entire
	// coercion walk — accumulation AND the alignment-skips a saturated slot
	// triggers. Finding 5 gives each precedence slot its OWN budget so
	// text>content>parts no longer depends on JSON key order; that means the
	// per-slot node cap can no longer double as the whole-walk CPU/allocation
	// bound, so this does. It is 4× maxCoerceParts: a fair three-slot
	// competition (text+content+parts, each up to maxCoerceParts) plus realign
	// overhead completes, while a hostile payload — a huge slot value, a giant
	// top-level array, a non-candidate key with a multi-MiB value — is abandoned
	// after a bounded constant of nodes, never proportional to the input
	// (finding 1). A lower-precedence slot larger than this bound can still
	// truncate the walk before a later key — the accepted bounded-work floor,
	// the same shape round-4 already had; the JS side has no such floor because
	// it reads only the winning property off a materialized object.
	maxCoerceVisits = 4 * maxCoerceParts
)

// coerceGuard is the SINGLE global bound shared across an entire coercion walk:
// one monotonic counter of VALUES touched plus the stop flag it trips. Because
// finding 5 gives each precedence slot its OWN coerceBudget (so text>content>
// parts no longer depends on JSON key order), the per-slot node cap can no
// longer bound whole-walk CPU/allocation — this does. Every walkFlexValue /
// skipFlexValue entry charges one visit; past maxCoerceVisits the flag trips and
// the whole walk unwinds, keeping what it already accumulated (truncate, never
// nuke). One guard is shared by the root accumulator and every slot beneath it,
// so total work stays a bounded constant no matter how the payload nests.
type coerceGuard struct {
	visits  int
	stopped bool
}

// coerceBudget bounds ONE accumulator's kept output: the fragment COUNT (nodes)
// and the JSON-WIRE BYTES (bytes) it may retain. The root accumulator and each
// of an object's three precedence slots (text/content/parts) hold their OWN
// coerceBudget so a lower-precedence key walked first (many `parts`, then
// `content`) can never spend the budget a later higher-precedence `text` needs
// (finding 5 — the round-4 SHARED budget made text>content>parts depend on JSON
// key order, diverging from the JS side). The winning slot is promoted into its
// parent by RE-CHARGING the parent's budget (appendFrom), so the result stays
// globally bounded (maxCoerceParts fragments / maxCoerceBytes) no matter how the
// fan-out nests, even though each slot counts on its own. Worst-case LIVE
// allocation is the three simultaneous slots — 3× budget — per level of nesting.
type coerceBudget struct {
	nodes int // fragments kept in THIS accumulator (bounds the injected newlines)
	bytes int // JSON-wire bytes kept in THIS accumulator
}

// coerceAcc accumulates joined text fragments for one scope. Every acc SHARES
// the one whole-walk coerceGuard but carries its OWN coerceBudget: the root's,
// or a fresh one per precedence slot (slotAcc). texts holds this scope's
// fragments; a slot is folded into its parent via appendFrom, which re-charges
// the parent's budget and truncates there if the parent lacks room.
type coerceAcc struct {
	guard  *coerceGuard
	budget *coerceBudget
	texts  []string
}

// newCoerceAcc returns a root accumulator with a fresh guard + budget. Slot
// accumulators are made via slotAcc (shared guard, fresh budget).
func newCoerceAcc() coerceAcc {
	return coerceAcc{guard: &coerceGuard{}, budget: &coerceBudget{}}
}

// slotAcc returns a fresh accumulator for one precedence slot: it SHARES the
// whole-walk guard (so total work stays globally bounded) but gets its OWN
// budget (so its accumulation can't be starved by a sibling slot walked first,
// finding 5).
func (a *coerceAcc) slotAcc() coerceAcc {
	return coerceAcc{guard: a.guard, budget: &coerceBudget{}}
}

// push appends one leaf text fragment to THIS accumulator, clamped to its OWN
// remaining node + byte budget. The budget is per-slot (finding 5), so pushing
// into the text slot never consumes the content/parts slots' budget or vice
// versa — precedence is decided AFTER every present slot has independently
// collected, so it can't depend on JSON key order. Bytes are counted in
// JSON-WIRE BYTES (the size the fragment occupies once re-marshalled into a JSON
// string — see jsonWireCost), NOT raw len(), mirroring the JS side's
// jsonWireByteLen budget exactly (finding 2). The join delimiter is NOT charged
// here (so a single over-budget leaf still clamps to exactly the byte budget);
// the node budget bounds the fragment COUNT — and thus the injected newlines —
// to maxCoerceParts. wirePrefix backs off to a rune boundary so a multibyte-rune
// cut never emits invalid UTF-8 (finding 3). Whole-walk CPU/allocation is
// bounded separately by the shared guard, so push needs no stop flag of its own.
func (a *coerceAcc) push(s string) {
	if s == "" {
		return
	}
	b := a.budget
	if b.nodes >= maxCoerceParts {
		return // this scope's fragment budget is spent
	}
	remaining := maxCoerceBytes - b.bytes
	if remaining <= 0 {
		return
	}
	if cost := jsonWireCost(s); cost < remaining {
		a.texts = append(a.texts, s)
		b.bytes += cost
		b.nodes++
		return
	}
	// Over budget: keep the longest prefix whose wire cost fits the remainder,
	// never splitting a rune, then mark the byte budget spent.
	prefix, used := wirePrefix(s, remaining)
	if prefix != "" {
		a.texts = append(a.texts, prefix)
		b.bytes += used
		b.nodes++
	}
	b.bytes = maxCoerceBytes // byte budget spent
}

// jsonWireCost returns the number of BYTES s occupies once marshalled into a
// JSON string on the wire — NOT its raw len(). JSON escaping EXPANDS some
// bytes: a control byte (<0x20) becomes \u00XX (6), a backslash or double quote
// becomes a 2-byte escape; every other byte costs 1, so a multibyte UTF-8 rune
// costs its width. Counting raw len() (finding 2) undercounts a backslash/
// control/NUL-heavy field. Mirrors the JS jsonWireByteLen; Go strings are valid
// UTF-8, so there is no lone-surrogate case.
func jsonWireCost(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x20:
			n += 6
		case c == '"' || c == '\\':
			n += 2
		default:
			n++
		}
	}
	return n
}

// runeWireCost returns the JSON-wire byte cost of one rune of the given UTF-8
// byte width (the per-rune form of jsonWireCost): a single-byte control is 6, a
// single-byte quote/backslash is 2, any other single byte is 1, and a multibyte
// rune costs its UTF-8 width (never escaped).
func runeWireCost(r rune, size int) int {
	if size == 1 {
		switch {
		case r < 0x20:
			return 6
		case r == '"' || r == '\\':
			return 2
		default:
			return 1
		}
	}
	return size
}

// wirePrefix returns the longest prefix of s whose total JSON-wire cost is
// <= max, never splitting a multibyte rune, plus that prefix's wire cost.
func wirePrefix(s string, max int) (string, int) {
	if max <= 0 {
		return "", 0
	}
	cost := 0
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		rc := runeWireCost(r, size)
		if cost+rc > max {
			break
		}
		cost += rc
		i += size
	}
	return s[:i], cost
}

// appendFrom promotes another accumulator's collected fragments into a, CHARGING
// a's OWN budget per fragment and truncating when a runs out of room.
// walkFlexObject uses it to fold the winning precedence slot into the parent.
// Slots hold INDEPENDENT budgets (finding 5), so — unlike the round-4
// shared-budget version — a promotion is NOT free: it re-charges the parent, and
// that is exactly what keeps the final result globally bounded (maxCoerceParts
// fragments / maxCoerceBytes) no matter how deep a hostile fan-out nests, since
// every level's winner must fit the level above (mirrors the guarantee the
// shared budget gave, now that the per-slot split gave it up). The last promoted
// fragment may be wire-cost-clamped to the parent's remaining bytes; src's
// losing sibling slots are simply dropped. The fragments themselves are shared
// string headers, so no subtree bytes are copied.
func (a *coerceAcc) appendFrom(src *coerceAcc) {
	for _, s := range src.texts {
		if a.budget.nodes >= maxCoerceParts || a.budget.bytes >= maxCoerceBytes {
			return
		}
		a.push(s)
	}
}

// walkFlexBytes is the entry point for the bounded coercion walk. It streams
// the JSON value in `data` through a json.Decoder TOKEN stream so the traversal
// bounds (depth / node-count / bytes) bind BEFORE materialization: a huge flat
// array is consumed element-by-element and abandoned the instant the node cap
// is hit, so allocation is proportional to the accumulated budget, NOT to the
// input size (finding 1 — the old json.Unmarshal-into-[]RawMessage path
// materialized every element before the 256-node cap could apply). It NEVER
// errors: a value that carries no text degrades to no fragments.
func walkFlexBytes(data []byte, acc *coerceAcc) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // numbers aren't text — skip float parsing
	walkFlexValue(dec, 0, acc)
}

// flexKind classifies the JSON value walkFlexValue just consumed so
// walkFlexObject can rank text>content>parts WITHOUT buffering any candidate
// into a json.RawMessage (finding 1). Only the two distinctions precedence
// needs are reported: whether the value was JSON null (candidate absent) and
// whether it was a JSON array (the shape `content`/`parts` must be to win).
type flexKind int

const (
	flexOther flexKind = iota // string / object / number / bool
	flexNull                  // JSON null
	flexArray                 // JSON array
)

// walkFlexValue reads EXACTLY ONE complete JSON value from dec and folds its
// text into acc under the shared shape contract — string | array (of the same)
// | object with `text`/`content`/`parts` — bounded by the whole-walk visit
// guard, per-slot fragment/byte budget, and depth. Nested BARE arrays and
// objects are traversed identically to the parsers.js side. A non-text leaf
// (number/bool/null/textless object) contributes nothing. It leaves the decoder
// aligned past the value it read (so a parent array/object stays token-aligned)
// UNLESS the shared guard has stopped, in which case the whole walk is unwinding
// and alignment no longer matters. A saturated per-slot budget only CLAMPS
// appending (push no-ops), never halts the read, so the parent still reaches its
// later keys — only the guard halts. The returned flexKind reports the consumed
// value's shape so walkFlexObject can apply precedence without materializing any
// candidate.
func walkFlexValue(dec *json.Decoder, depth int, acc *coerceAcc) flexKind {
	g := acc.guard
	if g.stopped {
		return flexOther
	}
	g.visits++
	if g.visits > maxCoerceVisits {
		g.stopped = true // whole-walk CPU/allocation ceiling: unwind, keep what we have
		return flexOther
	}
	if depth > maxCoerceDepth {
		// Too deep: drop this branch but consume its tokens so the parent stays
		// aligned (skipFlexValue is iterative and guard-charged, so discarding a
		// pathologically deep or huge subtree adds no Go stack depth and never
		// reads past the whole-walk ceiling).
		skipFlexValue(dec, g)
		return flexOther
	}
	tok, err := dec.Token()
	if err != nil {
		g.stopped = true // malformed/EOF: stop cleanly, keep what we have
		return flexOther
	}
	switch v := tok.(type) {
	case nil:
		return flexNull // JSON null
	case string:
		acc.push(v) // clamped to acc's OWN budget; a saturated slot no-ops but stays aligned
	case json.Delim:
		switch v {
		case '[':
			for dec.More() {
				if g.stopped {
					return flexArray // whole-walk stop: caller discards the decoder
				}
				walkFlexValue(dec, depth+1, acc)
			}
			if !g.stopped {
				_, _ = dec.Token() // consume closing ']'
			}
			return flexArray
		case '{':
			walkFlexObject(dec, depth+1, acc)
		}
	default:
		// json.Number / bool — not free-text content.
	}
	return flexOther
}

// walkFlexObject folds ONE JSON object into acc. It mirrors the JS coerceCollect
// precedence EXACTLY: a present, non-null `text` wins (recursed, so a
// pathological nested-array `.text` still resolves), else a `content` array,
// else a `parts` array. Because a streaming decoder sees keys in JSON SOURCE
// ORDER — `text` may arrive AFTER `content`/`parts` — each candidate is walked
// IN PLACE as the decoder reaches it, into its OWN slot accumulator, and
// precedence is applied only AFTER the object closes (the candidates are never
// captured as raw bytes; a json.RawMessage capture would be input-proportional,
// finding 1). Crucially each slot carries an INDEPENDENT budget (finding 5): the
// round-4 design SHARED one budget across the three slots, so a lower-precedence
// key walked first (many `parts`, then `content`) could exhaust the budget
// before `text` was reached, making the winner depend on key order and diverging
// from the JS side (which reads only the winning property off a materialized
// object, so it never even walks the losers). The three slots SHARE the
// whole-walk guard, so total work stays a bounded constant even though each
// budget counts on its own; worst-case LIVE allocation is the three slots at
// once — 3× budget — per level of nesting (finding 1's "bounded by a constant
// multiple of the budget"). At object close the highest-precedence non-empty
// slot is promoted into the parent via appendFrom, which RE-CHARGES the parent's
// budget and truncates there if the parent lacks room — so parent-level global
// accounting stays intact for everything OUTSIDE the slot competition, and the
// result stays globally bounded across any nesting. childDepth is the depth
// assigned to each candidate's value; non-candidate keys are skipped without
// materialization.
func walkFlexObject(dec *json.Decoder, childDepth int, acc *coerceAcc) {
	g := acc.guard
	// Each slot gets its OWN budget (finding 5) but SHARES the whole-walk guard,
	// so a lower-precedence key walked first can't starve a later higher-
	// precedence one, while total work stays globally bounded.
	var (
		textAcc    = acc.slotAcc()
		contentAcc = acc.slotAcc()
		partsAcc   = acc.slotAcc()

		textPresent  bool
		contentArray bool
		partsArray   bool
	)
	for dec.More() {
		if g.stopped {
			break // whole-walk stop: promote what we have, then unwind
		}
		keyTok, err := dec.Token()
		if err != nil {
			g.stopped = true
			break
		}
		key, _ := keyTok.(string)
		switch key {
		case "text":
			// text wins whenever present and non-null (even a scalar).
			textPresent = walkFlexValue(dec, childDepth, &textAcc) != flexNull
		case "content":
			contentArray = walkFlexValue(dec, childDepth, &contentAcc) == flexArray
		case "parts":
			partsArray = walkFlexValue(dec, childDepth, &partsAcc) == flexArray
		default:
			skipFlexValue(dec, g)
		}
	}
	if !g.stopped {
		_, _ = dec.Token() // consume closing '}' (only safe when still aligned)
	}
	// Promote the highest-precedence non-empty slot into the parent, RE-CHARGING
	// the parent's budget. This runs even after a whole-walk stop so a truncated
	// result still propagates up (truncate, never nuke).
	switch {
	case textPresent:
		acc.appendFrom(&textAcc)
	case contentArray:
		acc.appendFrom(&contentAcc)
	case partsArray:
		acc.appendFrom(&partsAcc)
	}
}

// skipFlexValue consumes exactly one complete JSON value from dec without folding
// any text — used for over-deep branches and non-candidate object keys. It tracks
// brace/bracket nesting ITERATIVELY (never adds Go stack depth) and charges the
// whole-walk guard per token, so discarding a pathologically deep or huge subtree
// is bounded by maxCoerceVisits, NOT by the input size (finding 1). On the guard
// ceiling it trips g.stopped and returns; the caller then unwinds and the decoder
// is discarded, so leaving it mid-value is harmless.
func skipFlexValue(dec *json.Decoder, g *coerceGuard) {
	if g.stopped {
		return
	}
	g.visits++
	if g.visits > maxCoerceVisits {
		g.stopped = true
		return
	}
	tok, err := dec.Token()
	if err != nil {
		g.stopped = true
		return
	}
	d, ok := tok.(json.Delim)
	if !ok || (d != '[' && d != '{') {
		return // scalar: one token consumed
	}
	depth := 1
	for depth > 0 {
		if g.stopped {
			return
		}
		g.visits++
		if g.visits > maxCoerceVisits {
			g.stopped = true
			return
		}
		t, err := dec.Token()
		if err != nil {
			g.stopped = true
			return
		}
		if dd, ok := t.(json.Delim); ok {
			switch dd {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
	}
}

// UnmarshalJSON decodes a CapturedTurn, routing the free-text content fields
// (prompt_text / response_text / title) through flexString so an array-shaped
// value degrades to a joined string instead of failing the whole turn. Every
// other field keeps encoding/json's default behavior via the embedded alias
// (a distinct type, so this method is NOT called recursively). The three flex
// fields are shadowed at depth 0 and win the JSON tag over the embedded
// alias's string fields; they are copied back onto the plain-string public
// fields after decode so the rest of the package sees ordinary strings. The
// shadow fields are SEEDED from the receiver's current values before decode:
// encoding/json only calls flexString.UnmarshalJSON when the key is PRESENT,
// so seeding preserves receiver-reuse semantics — decoding JSON that OMITS
// prompt_text/response_text/title leaves those existing receiver values
// intact instead of erasing them to "".
func (t *CapturedTurn) UnmarshalJSON(data []byte) error {
	type rawTurn CapturedTurn
	aux := struct {
		*rawTurn
		PromptText   flexString `json:"prompt_text"`
		ResponseText flexString `json:"response_text"`
		Title        flexString `json:"title"`
	}{
		rawTurn:      (*rawTurn)(t),
		PromptText:   flexString(t.PromptText),
		ResponseText: flexString(t.ResponseText),
		Title:        flexString(t.Title),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t.PromptText = string(aux.PromptText)
	t.ResponseText = string(aux.ResponseText)
	t.Title = string(aux.Title)
	return nil
}

// Granularity names how much of a captured turn the extension was
// configured to send (§5.3). It is a capability the normalizer reads from a
// rule table, not a per-site branch.
type Granularity string

const (
	// GranularityUsageOnly (the Phase-1 default): counts + model + latency,
	// NO content field is constructed. The safest posture — there is nothing
	// to scrub because there is no content.
	GranularityUsageOnly Granularity = "usage_only"
	// GranularityRedacted: content the extension client-side-redacted before
	// sending. Still hits the server scrub seam at ingest.
	GranularityRedacted Granularity = "redacted"
	// GranularityFull: raw content. Still hits the server scrub seam at
	// ingest (the DB never stores unscrubbed content).
	GranularityFull Granularity = "full"
)

// granularityRule is one row of the data-driven granularity table: whether a
// given level constructs content fields at all. redactedByServer is a
// documentation flag only — the server ALWAYS scrubs any content it stores;
// the client-side redaction that distinguishes redacted from full happens
// upstream, in the extension, before the wire.
type granularityRule struct {
	includeContent bool
}

// granularityRules is walked top-down by a lookup, never a switch. An
// unknown / empty granularity resolves to the usage-only floor.
var granularityRules = map[Granularity]granularityRule{
	GranularityUsageOnly: {includeContent: false},
	GranularityRedacted:  {includeContent: true},
	GranularityFull:      {includeContent: true},
}

// resolveGranularity maps the payload string to a rule, defaulting to the
// usage-only floor for empty / unknown values (fail-safe: never construct
// content the caller didn't explicitly ask for).
func resolveGranularity(s string) (Granularity, granularityRule) {
	g := Granularity(strings.TrimSpace(s))
	if rule, ok := granularityRules[g]; ok {
		return g, rule
	}
	return GranularityUsageOnly, granularityRules[GranularityUsageOnly]
}

// granularityRank orders the levels for the daemon-ceiling clamp (§5.1: the
// daemon is the final authority on what is STORED). A higher rank discloses
// more content. usage_only is the safe floor.
var granularityRank = map[Granularity]int{
	GranularityUsageOnly: 0,
	GranularityRedacted:  1,
	GranularityFull:      2,
}

// clampGranularity applies the daemon's ceiling (§5.1). The EFFECTIVE
// granularity is min(what the extension sent, the daemon ceiling): the
// daemon never trusts the extension to enforce privacy, so an extension that
// sends "full" while the daemon config caps at "usage_only" is downgraded to
// usage_only and no content is stored. An empty/unknown ceiling means "no
// ceiling" — store what the (already-resolved) client granularity allows.
// Both inputs are resolved through resolveGranularity first, so unknown
// values fail safe to usage_only.
func clampGranularity(client, ceiling Granularity) Granularity {
	if ceiling == "" {
		return client
	}
	cr, ok := granularityRank[ceiling]
	if !ok {
		// Unknown ceiling → fail safe to the usage-only floor.
		return GranularityUsageOnly
	}
	if granularityRank[client] <= cr {
		return client
	}
	return ceiling
}

// effectiveGranularity resolves the STORED granularity for a turn from the
// client's requested granularity and the daemon ceiling. It applies the §5.1
// rank clamp AND an honesty rule the rank clamp alone cannot express:
// "redacted" is a CLIENT-SIDE transform — the extension stripped content
// BEFORE it ever left the browser. The daemon has no redactor of its own (the
// ingest scrub seam is a generic secrets pass, not the granularity redaction);
// it can only legitimately LABEL stored content "redacted" when the client
// actually performed that redaction.
//
// The dangerous case the plain clamp mishandles: client sent "full" (raw
// content) while the ceiling caps at "redacted". Clamping only the LABEL would
// store raw content under a "redacted" label for a redaction that never
// happened — a lie about how the content was handled. The only honest
// downgrade available without an extension-side redaction pass is to drop the
// content entirely, so effective collapses to usage_only (metadata kept,
// content dropped). Client redacted→redacted and full→full are unchanged, and
// because a "full" client can only reach effective=="redacted" via a
// redacted ceiling, this fires exactly on the mislabel case.
//
// Empty/unknown client values resolve to the usage_only floor upstream (via
// resolveGranularity), so they never reach effective=="redacted" here — the
// conservative path holds.
func effectiveGranularity(client, ceiling Granularity) Granularity {
	eff := clampGranularity(client, ceiling)
	if eff == GranularityRedacted && client != GranularityRedacted {
		// The client did NOT redact (it sent full, or a usage_only floor that
		// never reaches here), but the ceiling forbids storing raw content.
		// We cannot fabricate the redaction — drop content, keep usage.
		return GranularityUsageOnly
	}
	return eff
}

// siteRule is one row of the per-site data table. tokenFamily selects the
// estimation heuristic hint (Phase 1 uses a single chars/4 estimator for
// every family; the field records the INTENDED client tokenizer so the
// server stays honest about what "estimated" means). transport records the
// wire shape the extension's MAIN-world parser taps (documentation only —
// the server treats every site identically; it is a per-site DATA cell, not
// a code branch). bestEffort marks a site whose parser is known-incomplete
// (Gemini's BatchExecute RPC), so the honesty surfaces in one place.
type siteRule struct {
	tool         string
	host         string
	defaultModel string
	tokenFamily  string
	transport    string
	bestEffort   bool
}

// siteRules is the site-as-data-discriminator table. Adding a *-web site is
// ONE new row here — no new package, no new code branch (CLAUDE.md #3/#5).
// The normalizer logic below is fully site-agnostic; the sites differ only
// by these table cells.
var siteRules = map[string]siteRule{
	models.ToolChatGPTWeb: {
		tool:         models.ToolChatGPTWeb,
		host:         "chatgpt.com",
		defaultModel: "gpt-4o",
		// The intended client-side tokenizer for the OpenAI family is
		// gpt-tokenizer (pure JS). The server-side fallback is chars/4.
		tokenFamily: "openai",
		transport:   "sse",
	},
	models.ToolClaudeWeb: {
		tool:         models.ToolClaudeWeb,
		host:         "claude.ai",
		defaultModel: "claude-sonnet-4-5",
		// Anthropic family. The official @anthropic-ai/tokenizer is
		// documented-inaccurate for Claude 3+; the client uses it with a
		// per-generation fudge factor, the server falls back to chars/4.
		tokenFamily: "anthropic",
		transport:   "sse",
	},
	models.ToolPerplexityWeb: {
		tool:         models.ToolPerplexityWeb,
		host:         "perplexity.ai",
		defaultModel: "sonar",
		// No dedicated light client tokenizer for Perplexity → chars/4
		// heuristic, always labeled estimated.
		tokenFamily: "heuristic",
		transport:   "sse",
	},
	models.ToolGeminiWeb: {
		tool:         models.ToolGeminiWeb,
		host:         "gemini.google.com",
		defaultModel: "gemini-2.5-flash",
		// No dedicated light client tokenizer for Gemini → chars/4.
		tokenFamily: "heuristic",
		// BatchExecute RPC — the hardest transport in the family. The
		// extension parser is BEST-EFFORT / incomplete; a turn that the
		// parser cannot fully decode still records whatever it extracts
		// (usage-only floor at minimum) rather than crashing.
		transport:  "batchexecute",
		bestEffort: true,
	},
	models.ToolCopilotWeb: {
		tool:         models.ToolCopilotWeb,
		host:         "copilot.microsoft.com",
		defaultModel: "copilot",
		tokenFamily:  "heuristic",
		// WebSocket frames (setOptions/send/appendText/done) — a distinct
		// parser from the SSE sites, cf_clearance-gated.
		transport: "websocket",
	},
}

// SourceFileFor returns the deterministic SourceFile sentinel for a site's
// extension-captured events (mirrors the "hermes:hook" convention). It marks
// the browser-extension capture path so rows are distinguishable from any
// future watcher/SQLite path and never collide on the UNIQUE index.
func SourceFileFor(site string) string {
	return site + ":extension"
}

// Parse decodes a captured-turn payload into a CapturedTurn. It does not
// validate — call BuildToolEvent / BuildTokenEvent (or Normalize) for the
// site/required-field checks.
func Parse(body []byte) (CapturedTurn, error) {
	var t CapturedTurn
	if err := json.Unmarshal(body, &t); err != nil {
		return CapturedTurn{}, fmt.Errorf("browserchat.Parse: %w", err)
	}
	return t, nil
}

// Options tunes normalization. It is the seam through which the daemon
// enforces its side of the §5.1 precedence: the extension is the authority
// on what is SENT, the daemon is the final authority on what is STORED.
type Options struct {
	// Scrubber is the ingest-time secrets scrubber applied to any content
	// that survives the granularity clamp. nil = no scrubbing (tests only;
	// production callers always pass a real scrubber).
	Scrubber *scrub.Scrubber
	// GranularityCeiling is the daemon's maximum stored granularity
	// (§5.1). The EFFECTIVE granularity of every turn is clamped to
	// min(what the extension sent, this ceiling). "" = no ceiling (store
	// what the client granularity allows). Set from [browser].granularity_
	// ceiling by the daemon; the CLI hook path leaves it "".
	GranularityCeiling Granularity
}

// Normalize parses and normalizes a captured-turn payload into the ToolEvent
// + TokenEvent slices the store.Ingest seam consumes. It is the single entry
// point the `observer browser hook` command and the loopback listener both
// call. An unknown site is an error (the caller logs and drops the turn —
// capture must never crash); a missing conversation id is NOT — the turn is
// recorded under a synthesized session key (sessionKeyFor). sc may be nil (no
// scrubbing), though production callers always pass a real scrubber.
func Normalize(body []byte, sc *scrub.Scrubber) ([]models.ToolEvent, []models.TokenEvent, error) {
	return NormalizeWith(body, Options{Scrubber: sc})
}

// NormalizeWith is Normalize with the full Options surface (the daemon's
// granularity ceiling). Both the hook command and the loopback listener
// funnel through this ONE owner of the normalize logic — the transport
// (native-messaging vs HTTP) is a deployment detail, not a schema fork.
func NormalizeWith(body []byte, opts Options) ([]models.ToolEvent, []models.TokenEvent, error) {
	turn, err := Parse(body)
	if err != nil {
		return nil, nil, err
	}
	var (
		toolEvents  []models.ToolEvent
		tokenEvents []models.TokenEvent
	)
	evs, err := buildToolEvents(turn, opts.Scrubber, opts.GranularityCeiling)
	if err != nil {
		return nil, nil, err
	}
	toolEvents = append(toolEvents, evs...)
	tok, ok, err := BuildTokenEvent(turn)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		tokenEvents = append(tokenEvents, tok)
	}
	return toolEvents, tokenEvents, nil
}

// BuildToolEvent maps a captured turn to the normalized ASSISTANT message
// ToolEvent for that turn. Returns (event, true, nil) for a recordable turn;
// an error only for an unknown site (a drop-the-turn condition the caller
// logs). A missing conversation id is recorded under a synthesized session
// key rather than dropped (sessionKeyFor).
//
// It is a thin wrapper over buildToolEvents that surfaces only the assistant
// event (the last element — at content granularity a paired user_prompt
// event precedes it). Callers that need the full chat shape (user row +
// assistant row) go through Normalize / NormalizeWith, which append every
// event buildToolEvents emits.
//
// The assistant event's ActionType is ActionAssistantMessage and RawToolName
// is "<site>.assistant_text" so the dashboard's assistant-text surface keys
// off the suffix exactly as the swept CLI adapters do. Its RawToolInput is
// intentionally EMPTY — the prompt lives on the separate user_prompt event so
// it is never stored twice; the response (→ ToolOutput) and API-call detail
// (→ Metadata) ride the assistant event.
func BuildToolEvent(turn CapturedTurn, sc *scrub.Scrubber) (models.ToolEvent, bool, error) {
	evs, err := buildToolEvents(turn, sc, "")
	if err != nil {
		return models.ToolEvent{}, false, err
	}
	// buildToolEvents always returns at least the assistant event, and the
	// assistant event is always last.
	return evs[len(evs)-1], true, nil
}

// buildToolEvents maps a captured turn to the normalized ToolEvent(s) that
// render it as a proper chat in the dashboard. It is the implementation
// shared by the exported BuildToolEvent (no ceiling) and NormalizeWith
// (daemon ceiling). ceiling clamps the EFFECTIVE granularity to
// min(client, ceiling); "" means no ceiling.
//
// Shape, by effective granularity:
//
//   - usage_only: ONE event — the assistant message, metadata-only (no
//     prompt/response content). A user row with no text is noise, so no
//     user_prompt event is emitted.
//   - redacted / full: the assistant message event AND (when the turn
//     carried prompt text) a paired ActionUserPrompt event, so the timeline
//     shows "user said X → assistant did Y". The two events split the
//     content: the prompt lands ONLY on the user_prompt event's
//     RawToolInput, the response ONLY on the assistant event's ToolOutput —
//     never stored twice.
//
// The two events of one turn carry DISTINCT SourceEventIDs (the assistant
// keeps sourceEventID(turn); the user_prompt appends ":user") so the
// (source_file, source_event_id) idempotency key in store.InsertActions can
// never dedup-swallow one against the other. The user_prompt event's
// MessageID is the "user:<id>" synthetic key the dashboard's message grouper
// expects; the assistant event keeps the turn's real message id (mid).
//
// The assistant event ALWAYS carries API-call detail Metadata (request_url,
// id_source, effective granularity, token estimates) — non-content fields
// that are safe even at usage_only. Content (prompt / response) is scrubbed +
// capped when the granularity rule allows it.
func buildToolEvents(turn CapturedTurn, sc *scrub.Scrubber, ceiling Granularity) ([]models.ToolEvent, error) {
	rule, ok := siteRules[turn.Site]
	if !ok {
		return nil, fmt.Errorf("browserchat.BuildToolEvent: unknown site %q", turn.Site)
	}
	// A missing conversation_id is NOT fatal: real logged-in new-chat
	// thinking turns arrive id-less. sessionKeyFor synthesizes a stable key
	// so the turn is recorded rather than silently dropped. Only an unknown
	// site (a genuine contract violation) is fatal.

	clientGran, _ := resolveGranularity(turn.Granularity)
	effective := effectiveGranularity(clientGran, ceiling)
	gran := granularityRules[effective]
	model := turn.Model
	if model == "" {
		model = rule.defaultModel
	}
	var (
		sourceFile = SourceFileFor(turn.Site)
		baseID     = sourceEventID(turn)
		sessKey    = sessionKeyFor(turn)
		root       = projectRoot(rule.host)
		ts         = resolveTimestamp(turn.CapturedAt)
		mid        = messageID(turn)
	)

	assistant := models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: baseID,
		SessionID:     sessKey,
		ProjectRoot:   root,
		Timestamp:     ts,
		Tool:          rule.tool,
		Model:         model,
		ActionType:    models.ActionAssistantMessage,
		Target:        model,
		Success:       true,
		DurationMs:    turn.LatencyMs,
		RawToolName:   turn.Site + ".assistant_text",
		MessageID:     mid,
		Metadata:      buildMetadata(turn, effective, sc),
	}

	var events []models.ToolEvent
	if gran.includeContent {
		// User-prompt event — emitted only when the turn carried prompt
		// text (a user row with no text is noise). The prompt lives HERE,
		// never on the assistant event, so it is not stored twice.
		if turn.PromptText != "" {
			prompt := turn.PromptText
			if sc != nil {
				prompt = sc.String(prompt)
			}
			prompt = contentcap.Cap(prompt, contentcap.DefaultMaxBytes)
			events = append(events, models.ToolEvent{
				SourceFile:    sourceFile,
				SourceEventID: baseID + ":user",
				SessionID:     sessKey,
				ProjectRoot:   root,
				Timestamp:     ts,
				Tool:          rule.tool,
				Model:         model,
				ActionType:    models.ActionUserPrompt,
				Target:        previewLine(prompt, previewMaxBytes),
				Success:       true,
				RawToolName:   turn.Site + ".user_prompt",
				RawToolInput:  prompt,
				MessageID:     "user:" + mid,
			})
		}
		// Assistant response content rides the assistant event.
		if turn.ResponseText != "" {
			resp := turn.ResponseText
			if sc != nil {
				resp = sc.String(resp)
			}
			assistant.ToolOutput = contentcap.Cap(resp, contentcap.DefaultMaxBytes)
			assistant.Target = previewLine(resp, previewMaxBytes)
		}
	}
	// The assistant event is always last so BuildToolEvent can surface it as
	// evs[len-1] and the timeline renders user → assistant in order.
	events = append(events, assistant)
	return events, nil
}

// allowedIDSources is the closed enum CapturedTurn.IDSource is documented to
// carry (see its doc comment): none, request, stream, resume, url, chain.
// This mirrors exactly what the extension emits today — resolveIdSource in
// browser-extension/src/parsers.js returns "none"|"request"|"stream", the
// content-main.js emitTurn override path adds "resume", and the Perplexity
// thread resolver adds "url"|"chain". A new legitimate value needs a row
// here, not a broadened check.
var allowedIDSources = map[string]bool{
	"none":    true,
	"request": true,
	"stream":  true,
	"resume":  true,
	"url":     true,
	"chain":   true,
}

// idSourceMaxLen bounds a legitimate id_source value defensively — the
// longest allowed value ("request") is 7 bytes, so anything this long is
// already outside the enum and the map lookup below would reject it anyway;
// this just avoids holding an unbounded string in memory/logs before that
// lookup runs.
const idSourceMaxLen = 32

// normalizeIDSource maps the extension-supplied id_source to the documented
// closed enum, defaulting unknown, empty, or oversized values to "none" — the
// exact value browser-health telemetry already recognizes as "no real
// conversation-id provenance was recovered" (cmd/observer/browser.go
// peekBrowserTurn / IDSourceNone). A stale or malformed bridge must never
// persist arbitrary text into action metadata (even at usage_only
// granularity, where this is the only extension-controlled string that
// survives); anything outside the allowed set is indistinguishable from a
// missing id_source and must read exactly like one, not like a new category.
func normalizeIDSource(raw string) string {
	if len(raw) > idSourceMaxLen || !allowedIDSources[raw] {
		return "none"
	}
	return raw
}

// NormalizeIDSource exports normalizeIDSource's closed-enum mapping for
// callers outside this package. cmd/observer/browser.go's health telemetry
// (the IDSourceNone counter) recognized only the literal raw "none" before
// this export existed, so a garbage/omitted id_source the adapter itself
// collapses to "none" incremented nothing there — this lets that caller
// apply the SAME normalization buildMetadata does, so the telemetry counter
// can't diverge from what actually lands in the DB (Go re-review LOW fix).
func NormalizeIDSource(raw string) string {
	return normalizeIDSource(raw)
}

// buildMetadata assembles the per-turn API-call detail carried on the
// assistant event's Metadata column: the upstream request URL/path, the
// extension's id_source provenance, the EFFECTIVE (post-clamp) granularity,
// and the estimated prompt/response token counts. These are non-content
// fields — safe at every granularity including usage_only. request_url is
// scrubbed as a backstop (it is a path, never prompt/response content, but a
// stray credential in a query param must never survive). id_source is
// normalized against the documented closed enum (normalizeIDSource) so a
// stale/malformed bridge can't persist arbitrary text. Returns nil when
// every field is zero (defensive — effective granularity is always non-empty,
// so in practice a browser assistant event always carries metadata).
func buildMetadata(turn CapturedTurn, effective Granularity, sc *scrub.Scrubber) *models.ActionMetadata {
	url := turn.RequestURL
	if sc != nil {
		url = sc.String(url)
	}
	m := models.ActionMetadata{
		RequestURL:        url,
		IDSource:          normalizeIDSource(turn.IDSource),
		Granularity:       string(effective),
		PromptTokensEst:   turn.PromptTokensEst,
		ResponseTokensEst: turn.ResponseTokensEst,
	}
	if m.IsZero() {
		return nil
	}
	return &m
}

// BuildTokenEvent maps a captured turn to a normalized TokenEvent. Tokens are
// ALWAYS estimated: the client estimate is preferred when present, otherwise
// the server recomputes a chars/4 heuristic over whatever content the
// granularity allowed it to keep. Returns (zero, false, nil) when no token
// signal is available at all (usage-only granularity with no client
// estimate). Source = TokenSourceEstimated, Reliability = ReliabilityUnreliable.
func BuildTokenEvent(turn CapturedTurn) (models.TokenEvent, bool, error) {
	rule, ok := siteRules[turn.Site]
	if !ok {
		return models.TokenEvent{}, false, fmt.Errorf("browserchat.BuildTokenEvent: unknown site %q", turn.Site)
	}
	// A missing conversation_id is NOT fatal here either — the token event
	// shares the SAME synthesized session key as the tool event (sessionKeyFor
	// is the one owner) so both land in the same session.

	input := turn.PromptTokensEst
	if input == 0 {
		input = estimateTokens(turn.PromptText)
	}
	output := turn.ResponseTokensEst
	if output == 0 {
		output = estimateTokens(turn.ResponseText)
	}
	if input == 0 && output == 0 {
		// Usage-only granularity with no client estimate and no content to
		// estimate over — nothing usable to record.
		return models.TokenEvent{}, false, nil
	}

	model := turn.Model
	if model == "" {
		model = rule.defaultModel
	}
	return models.TokenEvent{
		SourceFile:    SourceFileFor(turn.Site),
		SourceEventID: sourceEventID(turn) + ":tok",
		SessionID:     sessionKeyFor(turn),
		ProjectRoot:   projectRoot(rule.host),
		Timestamp:     resolveTimestamp(turn.CapturedAt),
		Tool:          rule.tool,
		Model:         model,
		InputTokens:   input,
		OutputTokens:  output,
		Source:        models.TokenSourceEstimated,
		Reliability:   models.ReliabilityUnreliable,
		MessageID:     messageID(turn),
	}, true, nil
}

// estimateTokens is the server-side fallback heuristic: ~1 token per 4
// characters (rune-counted). The extension's intended client-side estimator
// is gpt-tokenizer (OpenAI family); this heuristic only runs when the client
// sent no estimate, and every result is labeled estimated/unreliable.
func estimateTokens(text string) int64 {
	if text == "" {
		return 0
	}
	n := len([]rune(text))
	return int64((n + 3) / 4)
}

// projectRoot synthesizes the stable, obviously-synthetic browser://<host>
// project root so browser turns never collide with a real filesystem path.
func projectRoot(host string) string {
	return "browser://" + host
}

// sourceEventID composes a deterministic dedup key for a captured turn:
// <session-key>:<message_id-or-synthetic>. Combined with the
// "<site>:extension" SourceFile it is unique across re-fires. It reuses the
// SAME session key as the ToolEvent/TokenEvent so a re-fired id-less turn
// dedups against itself.
func sourceEventID(turn CapturedTurn) string {
	return sessionKeyFor(turn) + ":" + messageID(turn)
}

// KeyTier names which precedence tier sessionKeyFor used to derive a turn's
// session key. It is exposed so the capture-telemetry path can count a turn
// as "synthetic" using the SAME decision sessionKeyFor makes, never a
// re-derived guess (LOW-1).
type KeyTier int

const (
	// KeyTierConversation — the turn carried a real conversation id.
	KeyTierConversation KeyTier = iota
	// KeyTierMessage — no conversation id, but a real per-message id grouped
	// the turn (NOT synthetic).
	KeyTierMessage
	// KeyTierCapture — neither conversation nor message id, but the extension
	// stamped an opaque per-turn capture_id. It groups the ONE turn's tool +
	// token events but is NOT a real conversation, so it counts as
	// synthetic-tier for the telemetry counter (LOW-1).
	KeyTierCapture
	// KeyTierLastResort — the turn carried NONE of the above (only an old
	// pre-capture_id bridge reaches here). The captured_at millisecond alone
	// is the key: collision-possible but content-free — no privacy leak.
	KeyTierLastResort
)

// keyTierFor is the ONE OWNER of the "which tier?" decision. sessionKeyFor
// switches on it, and IsSyntheticSessionKey reads it, so the counted-as-
// synthetic set can never drift from the actually-synthesized set.
func keyTierFor(conversationID, messageID, captureID string) KeyTier {
	switch {
	case conversationID != "":
		return KeyTierConversation
	case messageID != "":
		return KeyTierMessage
	case captureID != "":
		return KeyTierCapture
	default:
		return KeyTierLastResort
	}
}

// IsSyntheticSessionKey reports whether a turn with these ids lands under a
// SYNTHESIZED session key (capture_id or the captured_at last-resort) rather
// than a real conversation or message id. It is the honest source of truth
// for the capture-telemetry "Synthetic" counter: a turn grouped by a real
// message id is NOT synthetic, but a turn carrying NEITHER a conversation nor
// a message id IS — even when an opaque capture_id groups its two events,
// because capture_id is not a real conversation (LOW-1). The decision depends
// only on conv/msg, so the capture-telemetry peek path (which never reads
// capture_id) stays authoritative.
func IsSyntheticSessionKey(conversationID, messageID string) bool {
	return keyTierFor(conversationID, messageID, "") >= KeyTierCapture
}

// sessionKeyFor derives the stable session key that a captured turn's
// ToolEvent and TokenEvent MUST both carry (the critical invariant: the two
// events for one turn have to land in the same session). It is the ONE OWNER
// of that derivation — buildToolEvents and BuildTokenEvent both call it, so
// they can never disagree.
//
// Precedence, most-to-least authoritative:
//  1. conversation_id — the real, stable, multi-turn session id.
//  2. message_id — a per-message id, when the extension tied the turn to one
//     but couldn't supply the conversation id (mid-stream handoff).
//  3. capture_id — the opaque, random per-turn id the extension stamps on
//     every turn (crypto.randomUUID). It groups THIS turn's tool + token
//     events without deriving from any content, so two genuinely-different
//     id-less turns (even captured in the SAME millisecond) get different keys
//     while a re-fire of the same turn dedups against itself. Keyed as
//     "<site>:cap:<capture_id>" for legibility (MED-1 / MED-3).
//  4. a "<site>:<captured_at_ms>" last-resort id — reached ONLY by an old
//     pre-capture_id bridge. It is content-free (so no privacy leak) but
//     collision-possible: two distinct id-less turns in the same millisecond
//     from such a bridge would share a key. That is an accepted degradation
//     for a legacy extension; a current extension always supplies capture_id.
//
// A per-turn synthetic id means one session per id-less turn. That is the
// deliberate, self-healing trade: an id-less turn is recorded (never silently
// dropped) as its own single-turn session, and once the extension starts
// supplying real conversation ids those turns thread into their real session
// again with no migration.
func sessionKeyFor(turn CapturedTurn) string {
	switch keyTierFor(turn.ConversationID, turn.MessageID, turn.CaptureID) {
	case KeyTierConversation:
		return turn.ConversationID
	case KeyTierMessage:
		return turn.MessageID
	case KeyTierCapture:
		return turn.Site + ":cap:" + turn.CaptureID
	default:
		return turn.Site + ":" + lastResortStamp(turn)
	}
}

// messageID returns the turn's message id, synthesizing a deterministic one
// (matching the synthetic session key) when the extension omitted it (older
// bridge versions / a turn the extension couldn't tie to a message id). It
// keys off the SAME tiers as sessionKeyFor so the tool and token events of one
// turn always agree.
func messageID(turn CapturedTurn) string {
	if turn.MessageID != "" {
		return turn.MessageID
	}
	if turn.CaptureID != "" {
		return "cap:" + turn.CaptureID
	}
	return "t" + lastResortStamp(turn)
}

// lastResortStamp is the deterministic, CONTENT-FREE stamp used only when a
// turn carried neither a conversation/message id nor a capture_id (an old
// pre-capture_id bridge). It is the captured_at millisecond alone (0 when
// absent/unparseable, NEVER time.Now()), so every derivation for one turn —
// sessionKeyFor / messageID / sourceEventID and the token event's copies —
// agrees byte-for-byte across calls. Collision-possible for two distinct
// same-ms legacy turns, but it embeds no content, so nothing content-derived
// crosses the org-push privacy boundary (MED-3).
func lastResortStamp(turn CapturedTurn) string {
	ms, _ := capturedAtMillis(turn)
	return strconv.FormatInt(ms, 10)
}

// capturedAtMillis parses captured_at (RFC3339 or unix seconds) to unix
// millis. Unlike resolveTimestamp it returns (0, false) — NEVER time.Now() —
// for an empty / unparseable value, so every synthesized key is deterministic
// across the multiple calls made for one turn (MED-1). resolveTimestamp keeps
// the now-fallback for the display Timestamp, where a stable key is not
// required.
func capturedAtMillis(turn CapturedTurn) (int64, bool) {
	s := strings.TrimSpace(turn.CapturedAt)
	if s == "" {
		return 0, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().UnixMilli(), true
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC().UnixMilli(), true
	}
	return 0, false
}

// resolveTimestamp parses captured_at as RFC3339 or unix seconds, falling
// back to now for empty / unparseable values (a capture without a usable
// timestamp is still worth recording at wall-clock). It is used ONLY for the
// display Timestamp on the event, never for a dedup key — key derivation goes
// through capturedAtMillis so it stays deterministic (MED-1).
func resolveTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().UTC()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC()
	}
	return time.Now().UTC()
}

// previewLine returns at most max bytes of s (byte-sliced, matching the
// existing adapter convention).
func previewLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// Adapter is the hook-only adapter registration for a browser-chatbot site.
// It carries NO watch paths — capture arrives through the native-messaging
// bridge / loopback listener, never the fsnotify watcher — so WatchPaths is
// nil and IsSessionFile always returns false. It exists to satisfy the
// defaults.Adapters() / integration-registry coupling (the registry-coverage
// guardrail) and to declare the tool name; the real work is the package's
// Normalize / BuildToolEvent / BuildTokenEvent functions the hook command
// calls directly.
type Adapter struct {
	site string
}

// New returns the ChatGPT browser adapter (tool "chatgpt-web").
func New() *Adapter {
	return &Adapter{site: models.ToolChatGPTWeb}
}

// NewFor returns a hook-only browser adapter for the given *-web site. The
// site MUST be one registered in siteRules — an unknown site would produce
// an adapter whose Normalize path rejects every turn, so callers pass a
// models.Tool*Web constant. This is how defaults.Adapters() registers the
// per-site adapters without a per-site constructor (the site is DATA).
func NewFor(site string) *Adapter {
	return &Adapter{site: site}
}

// Name returns the adapter's stable tool identifier.
func (a *Adapter) Name() string { return a.site }

// WatchPaths returns nil — this is a hook-only adapter (capture arrives via
// the browser extension's native-messaging bridge, not the file watcher).
func (a *Adapter) WatchPaths() []string { return nil }

// IsSessionFile always returns false — there is no on-disk session file for
// a browser turn.
func (a *Adapter) IsSessionFile(string) bool { return false }

// ParseSessionFile is a no-op that returns an empty result. It is never
// invoked by the watcher (WatchPaths is nil), but the Adapter interface
// requires it.
func (a *Adapter) ParseSessionFile(_ context.Context, _ string, fromOffset int64) (adapter.ParseResult, error) {
	return adapter.ParseResult{NewOffset: fromOffset}, nil
}
