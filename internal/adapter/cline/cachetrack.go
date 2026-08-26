package cline

import (
	"encoding/json"
	"strconv"

	"github.com/marmutapp/superbased-observer/internal/adapter/cacheobs"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// MaxBlocksPerSession caps the per-session Tier-2 accumulator's
// running block count. Matches the claudecode template's cap
// (same memory budget, same OOM guard).
const MaxBlocksPerSession = cacheobs.DefaultMaxBlocksPerSession

// buildCacheObservations walks the whole-file message array (the
// same slice ParseSessionFile decodes) and emits one
// CacheTurnObservation per message carrying token accounting
// (msg.Usage != nil or msg.Metrics != nil, mirroring
// tokenEventFor's own gate). cline/roo-code content blocks are the
// same Anthropic-shape schema as cline-cli's — text / thinking /
// tool_use / tool_result / image — so the canonical marshaller
// below mirrors clinecli's marshalCanonicalBlockTier2 byte-for-
// byte, with an added "image" case since cline surfaces pasted
// images cline-cli's fixture set never exercised.
//
// The file is re-read whole on every poll (not JSONL-appended), so
// this is called once per ParseSessionFile invocation over the
// full decoded msgs slice; store-level (source_file,
// source_event_id) idempotency dedupes across re-parses exactly
// like the TokenEvent path above.
func buildCacheObservations(msgs []rawMessage, path, sessionID string) []models.CacheTurnObservation {
	if len(msgs) == 0 {
		return nil
	}
	acc := cacheobs.New(MaxBlocksPerSession)
	var out []models.CacheTurnObservation
	for i := range msgs {
		msg := &msgs[i]
		blocks := decodeContent(msg.Content)
		acc.ObserveBlocks(accumulateCacheBlocksTier2(blocks, msg.Role))

		ev, ok := tokenEventFor(msg, path, sessionID, "", "", "", "", msg.resolvedModel(), parseMilliTimestamp(msg.Ts), i)
		if !ok {
			continue
		}
		usage := models.CacheUsage{
			NetInputTokens:      ev.InputTokens,
			OutputTokens:        ev.OutputTokens,
			CacheReadTokens:     ev.CacheReadTokens,
			CacheCreationTokens: ev.CacheCreationTokens,
		}
		if cacheobs.IsZeroUsage(usage) {
			continue
		}
		messageID := sessionID + ":" + strconv.Itoa(i)
		obs := acc.Emit(path, sessionID, messageID, msg.resolvedModel(), parseMilliTimestamp(msg.Ts), usage, false)
		// §15.3 boundary: cline's non-Anthropic providers (metrics-
		// shape messages, providerId="cline") route through
		// implicit-cache-shape gateways depending on the resolved
		// model. Anthropic-shape (msg.Usage != nil) messages always
		// stay Anthropic-routed in practice, but the overlay is
		// applied uniformly — cachetrack.IsImplicitCacheProvider
		// defaults to false (Anthropic-shape) for an unmatched/empty
		// provider string, so this is a no-op for the common case.
		obs = cacheobs.ApplyImplicitCacheOverlay(obs, "", msg.resolvedModel())
		out = append(out, obs)
	}
	return out
}

// accumulateCacheBlocksTier2 converts cline/roo-code's Anthropic-
// shape content blocks into deterministic CacheBlockMeta entries.
// Mirrors clinecli's marshaller (same wire schema); the "image"
// case is cline-specific (cline-cli's fixtures never carried a
// pasted image).
func accumulateCacheBlocksTier2(blocks []rawContentBlock, role string) []models.CacheBlockMeta {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]models.CacheBlockMeta, 0, len(blocks))
	for _, b := range blocks {
		canon, kind, ok := marshalCanonicalBlockTier2(b)
		if !ok {
			continue
		}
		out = append(out, models.CacheBlockMeta{
			LevelLabel:     "message", // R1: transcripts never expose tools/system
			Kind:           kind,
			CanonicalBytes: canon,
			Role:           role,
		})
	}
	return out
}

// marshalCanonicalBlockTier2 produces the deterministic JSON
// canonical form of one cline/roo-code content block. Returns
// (canonical bytes, engine kind label, ok=false on unknown block
// type). Each payload is an explicit STRUCT (never a map) so
// encoding/json's deterministic field order preserves the R3
// byte-stability invariant.
func marshalCanonicalBlockTier2(b rawContentBlock) (canon []byte, kind string, ok bool) {
	switch b.Type {
	case "text":
		return marshalTextLikeBlock("text", b.Text)
	case "thinking", "redacted_thinking":
		txt := b.Thinking
		if txt == "" && b.Type == "redacted_thinking" {
			txt = "[redacted thinking]"
		}
		return marshalTextLikeBlock("thinking", txt)
	case "tool_use":
		return marshalToolUseBlock(b)
	case "tool_result":
		return marshalToolResultBlock(b)
	case "image":
		// No text/binary payload folded into the chain — image
		// bytes are never stored. The block's presence (and
		// ordering) is what the chain needs to stay position-
		// accurate against the source transcript.
		payload := struct {
			Type string `json:"type"`
		}{Type: "image"}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "text", true
	default:
		return nil, "", false
	}
}

func marshalTextLikeBlock(kind, text string) (canon []byte, kindOut string, ok bool) {
	payload := struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}{Type: kind, Text: text}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false
	}
	return buf, kind, true
}

func marshalToolUseBlock(b rawContentBlock) (canon []byte, kind string, ok bool) {
	payload := struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	}{
		Type:  "tool_use",
		ID:    b.ID,
		Name:  b.Name,
		Input: b.Input,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false
	}
	return buf, "tool_use", true
}

func marshalToolResultBlock(b rawContentBlock) (canon []byte, kind string, ok bool) {
	payload := struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		IsError   bool            `json:"is_error,omitempty"`
		Content   json.RawMessage `json:"content,omitempty"`
	}{
		Type:      "tool_result",
		ToolUseID: b.ToolUseID,
		IsError:   b.IsError,
		Content:   b.Content,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false
	}
	return buf, "tool_result", true
}
