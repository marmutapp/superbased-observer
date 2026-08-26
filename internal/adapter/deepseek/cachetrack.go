package deepseek

import (
	"encoding/json"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/cacheobs"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// MaxBlocksPerSession caps the per-session Tier-2 accumulator's running
// block count. Matches the claudecode template's cap (same memory
// budget, same OOM guard).
const MaxBlocksPerSession = cacheobs.DefaultMaxBlocksPerSession

// accumulateCacheBlocks converts a DeepSeek content-block array
// (user/message's data.content, assistant/message's data.message.content,
// or tool/result's data.message.content) into deterministic
// CacheBlockMeta entries, in order. A block whose Type isn't one of the
// three shapes this adapter reads (text / tool-call / tool-result) is
// skipped rather than aborting the whole message — see the package doc
// for the confirmed block-kind inventory.
func accumulateCacheBlocks(blocks []contentBlock, role string) []models.CacheBlockMeta {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]models.CacheBlockMeta, 0, len(blocks))
	for _, b := range blocks {
		canon, kind, ok := marshalCanonicalBlock(b)
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

// marshalCanonicalBlock produces the deterministic JSON canonical form of
// one DeepSeek content block. Returns (canonical bytes, engine kind
// label, ok=false on an unrecognised block type). Each payload is an
// explicit STRUCT (never a map) so encoding/json's deterministic field
// order preserves the R3 byte-stability invariant.
func marshalCanonicalBlock(b contentBlock) (canon []byte, kind string, ok bool) {
	switch b.Type {
	case "text":
		payload := struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}{Type: "text", Text: b.Text}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "text", true
	case "tool-call":
		payload := struct {
			Type      string `json:"type"`
			ID        string `json:"id,omitempty"`
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		}{Type: "tool-call", ID: b.ID, Name: b.Name, Arguments: b.Arguments}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "tool_use", true
	case "tool-result":
		payload := struct {
			Type       string `json:"type"`
			ToolCallID string `json:"toolCallId,omitempty"`
			IsError    bool   `json:"isError,omitempty"`
			Text       string `json:"text,omitempty"`
		}{Type: "tool-result", ToolCallID: b.ToolCallID, IsError: b.IsError, Text: toolResultText(b)}
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, "", false
		}
		return buf, "tool_result", true
	default:
		return nil, "", false
	}
}

// emitCacheObservation drains cacheAcc's pending content-block delta
// into one CacheTurnObservation for an assistant/message's usage
// sibling, when that usage carries anything worth persisting. Returns
// nil (no observation) when u is nil/zero or the derived CacheUsage is
// all-zero.
//
// DeepSeek Harness never reports cache-CREATION tokens on the wire (see
// assistantUsage's doc comment in records.go) — CacheCreationTokens is
// always left at its zero value here, an honest reflection of the data
// this adapter actually observes, never a fabricated placeholder.
func emitCacheObservation(acc *cacheobs.Accumulator, path, sessionID, messageID, model string, ts time.Time, u *assistantUsage) *models.CacheTurnObservation {
	if u.isZero() {
		return nil
	}
	usage := models.CacheUsage{
		NetInputTokens:  u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
	}
	if cacheobs.IsZeroUsage(usage) {
		return nil
	}
	obs := acc.Emit(path, sessionID, messageID, model, ts, usage, false)
	// §15.3 boundary: DeepSeek Harness talks to the DeepSeek API
	// directly (a first-party CLI, not an OpenAI-gateway-shaped
	// provider), so ApplyImplicitCacheOverlay is a no-op for its model
	// strings — applied here only for consistency with every other
	// Tier-2 producer.
	obs = cacheobs.ApplyImplicitCacheOverlay(obs, "", model)
	return &obs
}
