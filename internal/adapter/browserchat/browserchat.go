package browserchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// call. An unknown site or a missing conversation id is an error (the caller
// logs and drops the turn — capture must never crash). sc may be nil (no
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
	ev, ok, err := buildToolEvent(turn, opts.Scrubber, opts.GranularityCeiling)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		toolEvents = append(toolEvents, ev)
	}
	tok, ok, err := BuildTokenEvent(turn)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		tokenEvents = append(tokenEvents, tok)
	}
	return toolEvents, tokenEvents, nil
}

// BuildToolEvent maps a captured turn to a normalized ToolEvent representing
// the assistant's response to one prompt. Returns (event, true, nil) for a
// recordable turn; an error for an unknown site or a missing conversation id
// (both are drop-the-turn conditions the caller logs).
//
// ActionType is ActionAssistantMessage and RawToolName is
// "<site>.assistant_text" so the dashboard's assistant-text surface keys off
// the suffix exactly as the swept CLI adapters do. Content (prompt →
// RawToolInput, response → ToolOutput) is constructed ONLY when the
// granularity rule allows it, and is scrubbed + capped when it is.
func BuildToolEvent(turn CapturedTurn, sc *scrub.Scrubber) (models.ToolEvent, bool, error) {
	return buildToolEvent(turn, sc, "")
}

// buildToolEvent is the implementation shared by the exported BuildToolEvent
// (no ceiling) and NormalizeWith (daemon ceiling). ceiling clamps the
// EFFECTIVE granularity to min(client, ceiling); "" means no ceiling.
func buildToolEvent(turn CapturedTurn, sc *scrub.Scrubber, ceiling Granularity) (models.ToolEvent, bool, error) {
	rule, ok := siteRules[turn.Site]
	if !ok {
		return models.ToolEvent{}, false, fmt.Errorf("browserchat.BuildToolEvent: unknown site %q", turn.Site)
	}
	if turn.ConversationID == "" {
		return models.ToolEvent{}, false, errors.New("browserchat.BuildToolEvent: conversation_id missing")
	}

	clientGran, _ := resolveGranularity(turn.Granularity)
	effective := clampGranularity(clientGran, ceiling)
	gran := granularityRules[effective]
	model := turn.Model
	if model == "" {
		model = rule.defaultModel
	}

	ev := models.ToolEvent{
		SourceFile:    SourceFileFor(turn.Site),
		SourceEventID: sourceEventID(turn),
		SessionID:     turn.ConversationID,
		ProjectRoot:   projectRoot(rule.host),
		Timestamp:     resolveTimestamp(turn.CapturedAt),
		Tool:          rule.tool,
		Model:         model,
		ActionType:    models.ActionAssistantMessage,
		Target:        rule.defaultModel,
		Success:       true,
		DurationMs:    turn.LatencyMs,
		RawToolName:   turn.Site + ".assistant_text",
		MessageID:     messageID(turn),
	}

	if gran.includeContent {
		if turn.PromptText != "" {
			prompt := turn.PromptText
			if sc != nil {
				prompt = sc.String(prompt)
			}
			ev.RawToolInput = contentcap.Cap(prompt, contentcap.DefaultMaxBytes)
		}
		if turn.ResponseText != "" {
			resp := turn.ResponseText
			if sc != nil {
				resp = sc.String(resp)
			}
			ev.ToolOutput = contentcap.Cap(resp, contentcap.DefaultMaxBytes)
			ev.Target = previewLine(resp, previewMaxBytes)
		}
	}
	return ev, true, nil
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
	if turn.ConversationID == "" {
		return models.TokenEvent{}, false, errors.New("browserchat.BuildTokenEvent: conversation_id missing")
	}

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
		SessionID:     turn.ConversationID,
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
// <conversation_id>:<message_id-or-synthetic>. Combined with the
// "<site>:extension" SourceFile it is unique across re-fires.
func sourceEventID(turn CapturedTurn) string {
	return turn.ConversationID + ":" + messageID(turn)
}

// messageID returns the turn's message id, synthesizing a timestamp-derived
// one when the extension omitted it (older bridge versions / a turn the
// extension couldn't tie to a message id).
func messageID(turn CapturedTurn) string {
	if turn.MessageID != "" {
		return turn.MessageID
	}
	return "t" + strconv.FormatInt(resolveTimestamp(turn.CapturedAt).UnixMilli(), 10)
}

// resolveTimestamp parses captured_at as RFC3339 or unix seconds, falling
// back to now for empty / unparseable values (a capture without a usable
// timestamp is still worth recording at wall-clock).
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
