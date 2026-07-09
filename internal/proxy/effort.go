package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Effort routing (§R6.5): downshift the request's effort/thinking
// fields instead of (or alongside) the model — zero cache loss, no
// tier-mapping risk; the lowest-risk enforce action.
//
// REPLACE-ONLY contract (G7): a field absent from the request is NEVER
// added — a model that rejects an unknown parameter would 4xx, turning
// a routing decision into a broken turn. Likewise the rewrite only
// ever DOWNSHIFTS: an existing setting at or below the target level is
// left untouched.

// effortRank orders the §R6.5 effort vocabulary.
var effortRank = map[string]int{
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
}

// anthropicThinkingBudgets maps effort levels to thinking budget_tokens
// targets. minimal disables thinking entirely; high is a no-op (no
// downshift). 1024 is Anthropic's documented minimum budget.
var anthropicThinkingBudgets = map[string]int64{
	"low":    4096,
	"medium": 8192,
}

// rewriteEffortFields applies an effort downshift to the request body,
// provider-aware:
//
//   - Anthropic: the top-level `thinking` object's budget_tokens is
//     lowered to the effort level's target (minimal → type:disabled).
//   - OpenAI: a top-level `reasoning_effort` string is lowered; a
//     Responses-API `reasoning` object's `effort` member likewise.
//
// Returns (body, false) — the ORIGINAL bytes — when there is nothing
// legal to change. The spliced span is the single field's value;
// every other byte is preserved (§R11.2).
func rewriteEffortFields(body []byte, effort, provider string) ([]byte, bool) {
	if effortRank[effort] == 0 {
		return body, false
	}
	switch provider {
	case "anthropic":
		return rewriteAnthropicThinking(body, effort)
	case "openai":
		if out, changed := rewriteOpenAIReasoningEffort(body, effort); changed {
			return out, true
		}
		return rewriteOpenAIReasoningObject(body, effort)
	default:
		return body, false
	}
}

// rewriteAnthropicThinking lowers an existing thinking budget.
func rewriteAnthropicThinking(body []byte, effort string) ([]byte, bool) {
	span, ok := topLevelValueSpan(body, "thinking")
	if !ok {
		return body, false // never add (G7)
	}
	var thinking struct {
		Type         string `json:"type"`
		BudgetTokens int64  `json:"budget_tokens"`
	}
	if err := json.Unmarshal(spanValueBytes(body, span), &thinking); err != nil {
		return body, false
	}
	if thinking.Type == "disabled" {
		return body, false // already at the floor
	}
	var replacement string
	if effort == "minimal" {
		replacement = `{"type":"disabled"}`
	} else {
		target, hasTarget := anthropicThinkingBudgets[effort]
		if !hasTarget {
			return body, false // high = no downshift
		}
		if thinking.BudgetTokens > 0 && thinking.BudgetTokens <= target {
			return body, false // already at or below the target
		}
		replacement = fmt.Sprintf(`{"type":"enabled","budget_tokens":%d}`, target)
	}
	return spliceSpan(body, span, replacement), true
}

// rewriteOpenAIReasoningEffort lowers a top-level reasoning_effort
// string (Chat Completions shape).
func rewriteOpenAIReasoningEffort(body []byte, effort string) ([]byte, bool) {
	span, ok := topLevelValueSpan(body, "reasoning_effort")
	if !ok {
		return body, false
	}
	var current string
	if err := json.Unmarshal(spanValueBytes(body, span), &current); err != nil {
		return body, false
	}
	if rank, known := effortRank[current]; known && rank <= effortRank[effort] {
		return body, false // already at or below the target
	}
	quoted, err := json.Marshal(effort)
	if err != nil {
		return body, false
	}
	return spliceSpan(body, span, string(quoted)), true
}

// rewriteOpenAIReasoningObject lowers a Responses-API `reasoning`
// object's effort member. The small object re-marshals (member order
// may normalize); everything outside its span is untouched.
func rewriteOpenAIReasoningObject(body []byte, effort string) ([]byte, bool) {
	span, ok := topLevelValueSpan(body, "reasoning")
	if !ok {
		return body, false
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(spanValueBytes(body, span), &reasoning); err != nil {
		return body, false
	}
	raw, has := reasoning["effort"]
	if !has {
		return body, false // never add a member the client didn't send
	}
	var current string
	if err := json.Unmarshal(raw, &current); err != nil {
		return body, false
	}
	if rank, known := effortRank[current]; known && rank <= effortRank[effort] {
		return body, false
	}
	quoted, _ := json.Marshal(effort)
	reasoning["effort"] = quoted
	replacement, err := json.Marshal(reasoning)
	if err != nil {
		return body, false
	}
	return spliceSpan(body, span, string(replacement)), true
}

// valueSpan is a top-level field's value byte range: [start, end).
type valueSpan struct {
	start int64 // after the key's closing quote (the splice re-emits ':')
	end   int64 // after the value's final byte
}

// spanValueBytes extracts the value-only bytes from a span (the span
// starts before the colon).
func spanValueBytes(body []byte, span valueSpan) []byte {
	b := body[span.start:span.end]
	if i := bytes.IndexByte(b, ':'); i >= 0 {
		b = b[i+1:]
	}
	return bytes.TrimSpace(b)
}

// spliceSpan replaces the span's value with replacement, re-emitting
// the colon (original inter-token whitespace inside the span is
// normalized away; all bytes outside it are verbatim).
func spliceSpan(body []byte, span valueSpan, replacement string) []byte {
	out := make([]byte, 0, len(body)+len(replacement))
	out = append(out, body[:span.start]...)
	out = append(out, ':')
	out = append(out, replacement...)
	out = append(out, body[span.end:]...)
	return out
}

// topLevelValueSpan locates the value span of a top-level object key
// using the same key/value-alternation scanner as
// rewriteTopLevelModel; the value may be any JSON value (the whole
// nested container is consumed). The remainder of the document is
// validated to EOF (with container-closure tracking) before the span
// is reported, so a splice can never alter a body that would not have
// parsed (G7).
func topLevelValueSpan(body []byte, key string) (valueSpan, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	type frame struct {
		isObject  bool
		expectKey bool
	}
	var stack []frame

	for {
		tok, err := dec.Token()
		if err != nil {
			return valueSpan{}, false
		}
		if d, isDelim := tok.(json.Delim); isDelim {
			switch d {
			case '{':
				stack = append(stack, frame{isObject: true, expectKey: true})
			case '[':
				stack = append(stack, frame{isObject: false})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return valueSpan{}, false
				}
				if top := &stack[len(stack)-1]; top.isObject {
					top.expectKey = true
				}
			}
			continue
		}
		if len(stack) == 0 {
			return valueSpan{}, false
		}
		top := &stack[len(stack)-1]
		if top.isObject && top.expectKey {
			k, _ := tok.(string)
			top.expectKey = false
			if len(stack) == 1 && k == key {
				start := dec.InputOffset()
				end, ok := consumeValue(dec)
				if !ok {
					return valueSpan{}, false
				}
				if !validateRemainder(dec, 1) {
					return valueSpan{}, false
				}
				return valueSpan{start: start, end: end}, true
			}
			continue
		}
		if top.isObject {
			top.expectKey = true
		}
	}
}

// consumeValue reads one complete JSON value (scalar or container)
// from the decoder and returns the offset after its final byte.
func consumeValue(dec *json.Decoder) (int64, bool) {
	tok, err := dec.Token()
	if err != nil {
		return 0, false
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return dec.InputOffset(), true
	}
	if d != '{' && d != '[' {
		return 0, false
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return 0, false
		}
		if dd, ok := tok.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return dec.InputOffset(), true
}

// validateRemainder drains the decoder to EOF, requiring every open
// container (openDepth at call time) to close.
func validateRemainder(dec *json.Decoder, openDepth int) bool {
	for {
		tok, err := dec.Token()
		if err != nil {
			return errors.Is(err, io.EOF) && openDepth == 0
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				openDepth++
			case '}', ']':
				openDepth--
			}
		}
	}
}
