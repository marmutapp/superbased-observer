package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicBlock is one entry inside an Anthropic message's `content` array.
// Only the fields conversation compression needs are decoded; unknown fields
// pass through via the RawEnc preserved on the containing message.
type anthropicBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	IsError      *bool           `json:"is_error,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// anthropicMessage is the per-entry shape of the request body's `messages`
// array. Content is either a bare string or an array of anthropicBlock.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// extractedMessage carries the parsed per-message detail the pipeline
// needs: the original raw JSON (for round-trip when untouched), the
// role, the compressible text (a tool_result body when present,
// flattened content otherwise), and the block-level metadata needed to
// rewrite the tool_result block when compression actually shrinks it.
type extractedMessage struct {
	raw  json.RawMessage
	role string
	// blocks is the decoded content array; empty when content was a bare
	// string.
	blocks []anthropicBlock
	// stringContent is set when content was a string rather than an
	// array.
	stringContent string
	// resultBlockIdx is the index of the single tool_result block we'll
	// compress, or -1 when no such block exists / there are multiple.
	resultBlockIdx int
	// resultText is the plain-text body of the tool_result block (bare
	// string or the concatenation of text blocks inside a structured
	// result array).
	resultText string
	// resultIsStructured is true when the tool_result's `content` is an
	// array of blocks; false when it's a bare string. Drives how the
	// compressed body is serialized back.
	resultIsStructured bool
	// toolUseIDs lists every tool_use_id this message produced. Multi-
	// element when the assistant emitted parallel tool calls (Claude
	// Code's "Read + LS + LS" pattern); single-element for the common
	// one-tool-per-turn case; empty for messages without tool_use.
	toolUseIDs []string
	// referencedIDs collects tool_use_ids from tool_result blocks.
	referencedIDs []string
	// flatText is the text version used for scoring.
	flatText string
}

// anthropicExtract parses an Anthropic Messages API request body. Returns
// the top-level envelope (every field preserved as RawMessage so the
// compressor can round-trip unknown top-level keys) and the extracted
// per-message detail slice. When the body isn't an Anthropic request at
// all (unknown shape, parse error), returns ok=false.
func anthropicExtract(body []byte) (envelope map[string]json.RawMessage, extracted []extractedMessage, ok bool) {
	envelope = make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil, false
	}
	raw, found := envelope["messages"]
	if !found {
		return nil, nil, false
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, nil, false
	}
	extracted = make([]extractedMessage, 0, len(msgs))
	for _, m := range msgs {
		var hdr anthropicMessage
		if err := json.Unmarshal(m, &hdr); err != nil {
			// Skip malformed messages but preserve the raw so the proxy
			// forwards the original body on re-serialize.
			extracted = append(extracted, extractedMessage{raw: m, resultBlockIdx: -1})
			continue
		}
		em := extractedMessage{
			raw:            m,
			role:           hdr.Role,
			resultBlockIdx: -1,
		}
		// Try array-of-blocks first; fall back to string.
		var blocks []anthropicBlock
		if err := json.Unmarshal(hdr.Content, &blocks); err == nil {
			em.blocks = blocks
			em.fillFromBlocks()
		} else {
			var s string
			if err := json.Unmarshal(hdr.Content, &s); err == nil {
				em.stringContent = s
				em.flatText = s
			}
		}
		extracted = append(extracted, em)
	}
	return envelope, extracted, true
}

// fillFromBlocks scans the decoded blocks to populate resultBlockIdx,
// resultText, toolUseID, referencedIDs, and flatText.
func (em *extractedMessage) fillFromBlocks() {
	var textParts []string
	resultIdxs := make([]int, 0, 2)
	for i, b := range em.blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			// Append (don't overwrite) so parallel tool_use blocks all
			// land in the slice. Pre-fix this dropped all but the last
			// id, which made parallel-tool-call messages invisible to
			// the budget enforcer's pair-integrity preservation logic.
			em.toolUseIDs = append(em.toolUseIDs, b.ID)
			textParts = append(textParts, fmt.Sprintf("tool_use %s(%s)", b.Name, string(b.Input)))
		case "tool_result":
			em.referencedIDs = append(em.referencedIDs, b.ToolUseID)
			resultIdxs = append(resultIdxs, i)
			resultText, structured := tool_resultText(b.Content)
			em.resultText = resultText
			em.resultIsStructured = structured
			textParts = append(textParts, resultText)
		}
	}
	if len(resultIdxs) == 1 {
		em.resultBlockIdx = resultIdxs[0]
	}
	em.flatText = strings.Join(textParts, "\n")
}

// tool_resultText decodes a tool_result block's `content` field. It can be
// a bare string or an array of `{type:"text", text:"..."}` entries. Returns
// the extracted text and whether the original shape was structured (an
// array).
func tool_resultText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n"), true
	}
	return "", false
}

// toConversationMessages projects extractedMessage → conversation.Message
// so the scorer + budget enforcer can operate on them.
func toConversationMessages(extracted []extractedMessage) []Message {
	msgs := make([]Message, len(extracted))
	for i, em := range extracted {
		text := em.flatText
		if em.resultBlockIdx >= 0 && em.resultText != "" {
			// When a message carries a compressible tool_result block,
			// scoring uses the result body itself — that's the payload
			// the compressor targets.
			text = em.resultText
		}
		msgs[i] = Message{
			Role:          normalizeAnthropicRole(em.role, em.resultBlockIdx >= 0),
			Text:          text,
			ByteLen:       len(em.raw),
			RawIndex:      i,
			Raw:           em.raw,
			ToolUseIDs:    em.toolUseIDs,
			ReferencedIDs: em.referencedIDs,
		}
	}
	return msgs
}

// normalizeAnthropicRole maps Anthropic's two roles ("user", "assistant")
// onto the scorer's four-role vocabulary. A user message that carries a
// tool_result block is effectively a tool message — the role-weight
// should reflect that it's mechanically compressible.
func normalizeAnthropicRole(role string, carriesToolResult bool) string {
	if carriesToolResult {
		return RoleTool
	}
	switch role {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	case "system":
		return RoleSystem
	}
	return role
}

// serializeAnthropic rebuilds a request body from the original envelope
// plus the post-enforcement Message slice. Messages with Raw != nil pass
// through untouched; messages with Text modified relative to the source
// extracted state have their single tool_result block rewritten to
// contain the compressed body; messages with Raw == nil (the drop-run
// markers) are serialized from scratch as user-role prose.
//
// When cacheBreakpointIdx is in range [0, len(final)), the last content
// block of that message is annotated with
// cache_control: {"type":"ephemeral"} to tell Anthropic's Messages API
// to cache everything up through that block (spec §10 Layer 3). A
// breakpoint of -1 disables injection.
func serializeAnthropic(envelope map[string]json.RawMessage, original []extractedMessage, final []Message, cacheBreakpointIdx int) ([]byte, error) {
	msgs := make([]json.RawMessage, 0, len(final))
	for _, m := range final {
		if m.Raw == nil {
			// Marker message — synthesize a minimal user-role content.
			obj, err := json.Marshal(map[string]any{
				"role":    "user",
				"content": m.Text,
			})
			if err != nil {
				return nil, fmt.Errorf("serializeAnthropic: marshal marker: %w", err)
			}
			msgs = append(msgs, obj)
			continue
		}
		// Raw is non-nil. If the message was compressed (Text shorter
		// than the original result text), rewrite the tool_result block.
		em := findByRaw(original, m.Raw)
		if em == nil || em.resultBlockIdx < 0 || m.Text == em.resultText {
			msgs = append(msgs, m.Raw)
			continue
		}
		// Rewrite the tool_result block's content with the compressed
		// Text. Preserve all other blocks and the rest of the message's
		// top-level fields.
		rewritten, err := rewriteToolResult(em, m.Text)
		if err != nil {
			// On failure, fall back to the original raw — better to
			// forward uncompressed than to corrupt the body.
			msgs = append(msgs, m.Raw)
			continue
		}
		msgs = append(msgs, rewritten)
	}
	if cacheBreakpointIdx >= 0 && cacheBreakpointIdx < len(msgs) {
		if annotated, err := injectCacheControl(msgs[cacheBreakpointIdx]); err == nil {
			msgs[cacheBreakpointIdx] = annotated
		}
		// On failure we leave the message as-is. Anthropic will just
		// skip the cache; nothing else is corrupted.
	}
	envelope["messages"], _ = json.Marshal(msgs)
	return marshalEnvelope(envelope)
}

// injectCacheControl annotates the last content block of an Anthropic
// message JSON with cache_control: {"type":"ephemeral"}. When the
// content is a bare string, it is first upgraded to a one-element
// [{"type":"text","text":"..."}] array so the annotation has somewhere
// to land. Existing cache_control on the target block is preserved
// verbatim. Returns the original bytes unchanged when the shape is not
// recognized so callers can forward safely.
func injectCacheControl(raw json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, err
	}
	contentRaw, ok := obj["content"]
	if !ok {
		return raw, nil
	}
	// Try array-of-blocks first — preserve unknown block fields by
	// decoding each block as map[string]any.
	var blocks []map[string]any
	if err := json.Unmarshal(contentRaw, &blocks); err == nil && len(blocks) > 0 {
		last := blocks[len(blocks)-1]
		if _, already := last["cache_control"]; !already {
			last["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		newContent, err := json.Marshal(blocks)
		if err != nil {
			return raw, err
		}
		obj["content"] = newContent
		return marshalEnvelope(obj)
	}
	// Fall back to bare string — upgrade to a text-block array so the
	// cache_control annotation has a valid home.
	var s string
	if err := json.Unmarshal(contentRaw, &s); err == nil {
		upgraded := []map[string]any{{
			"type":          "text",
			"text":          s,
			"cache_control": map[string]string{"type": "ephemeral"},
		}}
		newContent, err := json.Marshal(upgraded)
		if err != nil {
			return raw, err
		}
		obj["content"] = newContent
		return marshalEnvelope(obj)
	}
	// Unknown content shape — leave alone.
	return raw, nil
}

// rewriteToolResult returns a new raw JSON object for the given extracted
// message with its single tool_result block's content set to compressed.
// All other top-level fields on the message are preserved by round-tripping
// through a map decode.
func rewriteToolResult(em *extractedMessage, compressed string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(em.raw, &obj); err != nil {
		return nil, err
	}
	// Re-marshal the blocks slice with the target block's content replaced.
	blocks := make([]json.RawMessage, len(em.blocks))
	for i, b := range em.blocks {
		if i == em.resultBlockIdx {
			rewritten, err := marshalToolResultBlock(b, compressed, em.resultIsStructured)
			if err != nil {
				return nil, err
			}
			blocks[i] = rewritten
			continue
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		blocks[i] = raw
	}
	contentBytes, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	obj["content"] = contentBytes
	return marshalEnvelope(obj)
}

// marshalToolResultBlock rebuilds a single tool_result block with the
// supplied compressed body. When the original body was a structured
// array, the rewrite keeps the array shape with a single text entry;
// otherwise it emits a bare string.
func marshalToolResultBlock(b anthropicBlock, compressed string, structured bool) ([]byte, error) {
	out := map[string]any{
		"type":        "tool_result",
		"tool_use_id": b.ToolUseID,
	}
	if b.IsError != nil {
		out["is_error"] = *b.IsError
	}
	if len(b.CacheControl) > 0 {
		out["cache_control"] = b.CacheControl
	}
	if structured {
		out["content"] = []any{
			map[string]string{"type": "text", "text": compressed},
		}
	} else {
		out["content"] = compressed
	}
	return json.Marshal(out)
}

// marshalEnvelope re-emits a map of json.RawMessage as compact JSON in
// insertion-sorted key order (for determinism) with a stable
// `messages` ordering (already ordered by the caller).
//
// Anthropic is insensitive to top-level key order, but deterministic
// output makes the SHA-256 system_prompt_hash / message_prefix_hash
// stable across proxy restarts — valuable for cache-hit analysis.
func marshalEnvelope(obj map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	// json.Marshal on a map already produces sorted keys for string
	// keys, but we're using RawMessage values so we need to assemble the
	// object by hand.
	// Use a plain json.Encoder which will sort for us when given a map.
	tmp := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		tmp[k] = v
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tmp); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// findByRaw returns the extractedMessage whose raw bytes equal target, or
// nil when not found. Linear scan — request bodies rarely carry more than
// a few dozen messages.
func findByRaw(ex []extractedMessage, target json.RawMessage) *extractedMessage {
	for i := range ex {
		if bytes.Equal(ex[i].raw, target) {
			return &ex[i]
		}
	}
	return nil
}
