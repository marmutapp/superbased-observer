package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// openaiMessage is the Chat Completions per-message shape. Only the
// fields conversation compression needs are decoded; everything else
// rides through on the preserved Raw bytes.
type openaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// openaiContentPart covers the common parts: text and image_url. Other
// part types (audio, file) are preserved verbatim via the containing
// message's Raw bytes — they just don't contribute scoring text.
type openaiContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// openaiExtractedMessage is the per-message parsed state — analogous
// to extractedMessage in anthropic.go.
type openaiExtractedMessage struct {
	raw        json.RawMessage
	role       string
	toolCallID string
	// contentString is populated when the content field was a bare
	// string; contentParts when it was an array. Exactly one is set
	// for a well-formed message.
	contentString string
	contentParts  []openaiContentPart
	// flatText is the scorer input — the bare string, or the
	// concatenation of text parts.
	flatText string
	// isToolResult is true when role == "tool" (a Chat Completions
	// tool output). The enforcer targets these for per-type
	// compression the same way Anthropic targets tool_result blocks.
	isToolResult bool
}

// openaiExtract parses a Chat Completions request body. Returns ok=false
// when the body isn't a recognizable envelope (no parse, no messages
// array). Responses API bodies (`{input: [...]}`) fall into that bucket
// today — a future adapter can add a second detection path.
func openaiExtract(body []byte) (envelope map[string]json.RawMessage, extracted []openaiExtractedMessage, ok bool) {
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
	extracted = make([]openaiExtractedMessage, 0, len(msgs))
	for _, m := range msgs {
		var hdr openaiMessage
		if err := json.Unmarshal(m, &hdr); err != nil {
			// Preserve raw so serialization forwards the original.
			extracted = append(extracted, openaiExtractedMessage{raw: m})
			continue
		}
		em := openaiExtractedMessage{
			raw:          m,
			role:         hdr.Role,
			toolCallID:   hdr.ToolCallID,
			isToolResult: hdr.Role == "tool",
		}
		// Content can be a bare string, an array of parts, or null
		// (assistant messages that are pure tool_calls). Try string
		// first since it's the common case.
		var s string
		if err := json.Unmarshal(hdr.Content, &s); err == nil {
			em.contentString = s
			em.flatText = s
		} else {
			var parts []openaiContentPart
			if err := json.Unmarshal(hdr.Content, &parts); err == nil {
				em.contentParts = parts
				texts := make([]string, 0, len(parts))
				for _, p := range parts {
					if p.Type == "text" && p.Text != "" {
						texts = append(texts, p.Text)
					}
				}
				em.flatText = strings.Join(texts, "\n")
			}
			// null content (pure tool_calls message): flatText stays "".
		}
		extracted = append(extracted, em)
	}
	return envelope, extracted, true
}

// openaiToConversationMessages projects openaiExtractedMessage →
// conversation.Message so the shared scorer + budget enforcer can run.
func openaiToConversationMessages(extracted []openaiExtractedMessage) []Message {
	msgs := make([]Message, len(extracted))
	for i, em := range extracted {
		msg := Message{
			Role:     normalizeOpenAIRole(em.role, em.isToolResult),
			Text:     em.flatText,
			ByteLen:  len(em.raw),
			RawIndex: i,
			Raw:      em.raw,
		}
		if em.isToolResult && em.toolCallID != "" {
			// Tool output messages reference their assistant tool_call
			// via tool_call_id — mirror Anthropic's referencedIDs so
			// the scorer's reference-weight kicks in.
			msg.ReferencedIDs = []string{em.toolCallID}
		}
		msgs[i] = msg
	}
	return msgs
}

// normalizeOpenAIRole maps Chat Completions roles onto the scorer's
// four-role vocabulary. role="tool" is the OpenAI equivalent of an
// Anthropic user-message-carrying-tool_result and is what per-type
// compression targets.
func normalizeOpenAIRole(role string, isToolResult bool) string {
	if isToolResult {
		return RoleTool
	}
	switch role {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	case "system":
		return RoleSystem
	case "tool":
		return RoleTool
	}
	return role
}

// serializeOpenAI rebuilds a Chat Completions body from the original
// envelope + the post-enforcement Message slice. Messages with Raw !=
// nil pass through untouched when their Text is unchanged; tool-result
// messages whose Text shrunk have their content field rewritten;
// Raw == nil means the message is a compression marker.
//
// Chat Completions has no standardized cache marker today, so
// cacheBreakpointIdx is accepted for API parity with serializeAnthropic
// but intentionally ignored. A future Responses-API adapter can use
// this slot.
func serializeOpenAI(envelope map[string]json.RawMessage, original []openaiExtractedMessage, final []Message, _ int) ([]byte, error) {
	msgs := make([]json.RawMessage, 0, len(final))
	for _, m := range final {
		if m.Raw == nil {
			// Marker message — synthesize as user-role prose.
			obj, err := json.Marshal(map[string]any{
				"role":    "user",
				"content": m.Text,
			})
			if err != nil {
				return nil, fmt.Errorf("serializeOpenAI: marshal marker: %w", err)
			}
			msgs = append(msgs, obj)
			continue
		}
		em := findOpenAIByRaw(original, m.Raw)
		// Only rewrite when it's a tool-result message whose compressed
		// Text differs from the original content — mirrors the
		// Anthropic rule that keeps non-tool messages verbatim.
		if em == nil || !em.isToolResult || m.Text == em.flatText {
			msgs = append(msgs, m.Raw)
			continue
		}
		rewritten, err := rewriteOpenAIContent(em, m.Text)
		if err != nil {
			// Never poison the forward path — fall back to original.
			msgs = append(msgs, m.Raw)
			continue
		}
		msgs = append(msgs, rewritten)
	}
	envelope["messages"], _ = json.Marshal(msgs)
	return marshalEnvelope(envelope)
}

// rewriteOpenAIContent returns a new raw JSON object for em with its
// content field replaced by the compressed body. When the original
// content was a part array, the rewrite keeps the array shape with a
// single text entry; when it was a bare string, the rewrite emits a
// bare string.
func rewriteOpenAIContent(em *openaiExtractedMessage, compressed string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(em.raw, &obj); err != nil {
		return nil, err
	}
	var contentBytes []byte
	var err error
	if len(em.contentParts) > 0 {
		contentBytes, err = json.Marshal([]map[string]string{
			{"type": "text", "text": compressed},
		})
	} else {
		contentBytes, err = json.Marshal(compressed)
	}
	if err != nil {
		return nil, err
	}
	obj["content"] = contentBytes
	return marshalEnvelope(obj)
}

// findOpenAIByRaw returns the extractedMessage whose raw bytes equal
// target, or nil when not found. Linear scan — a single request rarely
// carries more than a few dozen messages.
func findOpenAIByRaw(ex []openaiExtractedMessage, target json.RawMessage) *openaiExtractedMessage {
	for i := range ex {
		if bytes.Equal(ex[i].raw, target) {
			return &ex[i]
		}
	}
	return nil
}
