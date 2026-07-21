package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Channel B seam (model-routing spec §R5, §R11): ONE optional Options
// field, plain proxy-local types on both sides — no routing-package
// types cross this boundary (§24.2; the Compressor precedent). The
// implementation lives at the cmd/observer boundary, where the routing
// engine, the snapshot refresher, and the decision-log store seam meet.
//
// Hot-path contract (§R9.2): Decide is called once per proxied request
// pre-forward; implementations read in-memory snapshots only (p99 <
// 5 ms) and fail open INTERNALLY — and the proxy additionally guards
// the call (panic recovery, empty/equal model no-op, splice failure →
// forward original) so routing can never break a turn that would have
// succeeded (G7).

// RouterShape is the request view the proxy resolves for the router —
// counts, hashes, and flags only, never body content.
type RouterShape struct {
	Model            string
	MessageCount     int
	ToolUseCount     int
	SystemPromptHash string
	Stream           bool
	Speed            string
	ServiceTier      string
	// PromptTokensEstimate is a coarse pre-response prompt-size
	// estimate (body bytes / 4) — enough for long-context banding and
	// context-window legality; the realized count lands on the turn
	// row afterwards.
	PromptTokensEstimate int64
}

// RouterSession is the transport-resolved per-request context.
type RouterSession struct {
	// Provider is the upstream wire family (models.ProviderAnthropic /
	// models.ProviderOpenAI).
	Provider string
	// SessionID is the resolved session id ("" when unresolvable).
	SessionID string
	// Entitlement is the §R11.3 entitlement class resolved from the
	// request's auth headers: "api_key", "subscription", or ""
	// (unknown — implementations treat it restrictively).
	Entitlement string
}

// RouterVerdict is the router's plain answer.
type RouterVerdict struct {
	// Apply commits the rewrite (enforce mode, legal, changed). When
	// false the proxy touches NOTHING — advise mode logs rows on the
	// implementation side only.
	Apply bool
	// SelectedModel is the model to serve when Apply is set.
	SelectedModel string
	// SetEffort, when non-empty and Apply is set, downshifts the
	// request's effort fields instead of / alongside the model
	// (§R6.5): minimal | low | medium | high.
	SetEffort string
	// FallbackModels is the ordered same-shape §R12.1 chain the retry
	// layer may try on 429/5xx/timeout.
	FallbackModels []string
	// Token identifies this decision for RecordServed linkage. Zero
	// means the router has nothing to link (no record kept).
	Token int64
}

// ModelRouter is the Channel-B seam interface (§R5).
type ModelRouter interface {
	// Decide evaluates one proxied request pre-forward.
	Decide(shape RouterShape, sess RouterSession) RouterVerdict
	// RecordServed links a decision to its api_turns row once the turn
	// lands (called off the hot path, on the detached insert
	// goroutine). servedModel is the model that actually served the
	// request. Turns that never land (dropped zero-usage streams) are
	// the implementation janitor's problem — RecordServed is then
	// simply never called for that token.
	RecordServed(token int64, apiTurnID int64, servedModel string)
}

// safeDecide invokes the router with a panic guard: a routing failure
// must never break the turn (G7) — the request proceeds unrouted.
func (p *Proxy) safeDecide(shape RouterShape, sess RouterSession) (v RouterVerdict) {
	defer func() {
		if rec := recover(); rec != nil {
			p.logger.Warn("proxy: model router panicked; failing open", "panic", rec)
			v = RouterVerdict{}
		}
	}()
	return p.router.Decide(shape, sess)
}

// recordServedSafe reports the landed turn to the router with the same
// panic guard — persistence side effects never propagate.
func (p *Proxy) recordServedSafe(token, apiTurnID int64, servedModel string) {
	if p.router == nil || token == 0 {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			p.logger.Warn("proxy: model router RecordServed panicked", "panic", rec)
		}
	}()
	p.router.RecordServed(token, apiTurnID, servedModel)
}

// applyRoutedBody installs the routed plain-JSON body as the forward
// payload, re-encoding to zstd when the client sent zstd. Returns
// false (leaving reqBody untouched) when re-encoding fails — fail-open.
func (p *Proxy) applyRoutedBody(r *http.Request, mutated []byte, reqBody *[]byte) bool {
	if isZstdEncoded(r.Header.Get("Content-Encoding")) {
		encoded, err := encodeZstd(mutated)
		if err != nil {
			p.logger.Warn("proxy: zstd re-encode routed body failed; forwarding original", "err", err)
			return false
		}
		*reqBody = encoded
		return true
	}
	*reqBody = mutated
	return true
}

// resolveEntitlement classifies the request's credentials (§R11.3):
//
//   - x-api-key header                  → api_key (Anthropic key auth)
//   - Authorization: Bearer sk-ant-oat… → subscription (Claude OAuth)
//   - Authorization: Bearer sk-…        → api_key (platform keys)
//   - Authorization: Bearer eyJ…        → subscription (ChatGPT-plan JWT)
//   - anything else                     → "" (unknown; routers treat
//     unknown as the restrictive class)
func resolveEntitlement(r *http.Request) string {
	if r.Header.Get("x-api-key") != "" {
		return "api_key"
	}
	authz := r.Header.Get("Authorization")
	const bearer = "Bearer "
	if !strings.HasPrefix(authz, bearer) {
		return ""
	}
	token := authz[len(bearer):]
	switch {
	case strings.HasPrefix(token, "sk-ant-oat"):
		return "subscription"
	case strings.HasPrefix(token, "sk-"):
		return "api_key"
	case strings.HasPrefix(token, "eyJ"):
		return "subscription"
	default:
		return ""
	}
}

// rewriteTopLevelModel splices a new value into the request body's
// top-level "model" field, leaving every other byte of the document
// intact (§R11.2: all non-model fields byte-identical — metadata /
// session identifiers / stream flag / cache_control untouched; only
// the model value's span is re-emitted). The shared topLevelValueSpan
// scanner (effort.go) tracks object key/value alternation with an
// explicit container stack — a "model" key nested inside message
// content can never be spliced — and validates the whole document
// before reporting a span. ok=false on any anomaly — the caller
// forwards the original body untouched (fail-open, G7).
func rewriteTopLevelModel(body []byte, newModel string) ([]byte, bool) {
	span, ok := topLevelValueSpan(body, "model")
	if !ok {
		return nil, false
	}
	var current string
	if err := json.Unmarshal(spanValueBytes(body, span), &current); err != nil {
		return nil, false // model value isn't a string; refuse
	}
	quoted, err := json.Marshal(newModel)
	if err != nil {
		return nil, false
	}
	return spliceSpan(body, span, string(quoted)), true
}
