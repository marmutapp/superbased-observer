package cowork

import (
	"encoding/json"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/cacheobs"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// MaxBlocksPerSession caps the per-session Tier-2 accumulator's
// running block count. Matches the claudecode template's cap (same
// memory budget, same OOM guard).
const MaxBlocksPerSession = cacheobs.DefaultMaxBlocksPerSession

// emitCacheObservationsTier2 builds one CacheTurnObservation per
// model in a `result` record's modelUsage map, draining cacheAcc's
// running per-session content-block delta.
//
// cowork's authoritative per-turn token accounting lives ONLY on the
// `result` record's modelUsage map (see handleResult's doc comment —
// assistant.message.usage is a streaming snapshot that drops
// output_tokens on most chunks and never sees internally-dispatched
// haiku calls). Content blocks, however, stream in on the "user" and
// "assistant" records that precede each result; handleUser and
// handleAssistant feed cacheAcc as they're parsed so the chain stays
// in transcript order regardless of which record carries usage.
//
// modelUsage is usually a single-entry map, but cowork can dispatch
// an internal model (e.g. a haiku sub-call for a background task)
// alongside the primary model within one result — the fixture-
// validated case carries both claude-opus-4-6 and
// claude-haiku-4-5-20251001 in the same result. sortedModels fixes
// iteration order (handleResult already sorts it for the TokenEvent
// loop; reused here for the same determinism reason). Because
// Accumulator.Emit drains and resets the pending delta, only the
// FIRST model in sorted order receives the turn's new BlockHashes;
// subsequent models in the same result emit with an empty delta
// (a valid, cheap no-op turn — never a fabricated one). This avoids
// crediting one turn's content blocks into two independent chains,
// and mirrors the ordering the TokenEvent path already commits to.
func emitCacheObservationsTier2(
	acc *cacheobs.Accumulator,
	path, sessionID string,
	rec rawRecord,
	ts time.Time,
	modelUsage map[string]rawResultModelUsage,
	tier1hFrac float64,
	sortedModels []string,
) []models.CacheTurnObservation {
	if len(sortedModels) == 0 {
		return nil
	}
	var out []models.CacheTurnObservation
	for _, model := range sortedModels {
		mu, ok := modelUsage[model]
		if !ok {
			continue
		}
		cw := mu.CacheCreationInputTokens
		cw1h := int64(float64(cw) * tier1hFrac)
		if cw1h > cw {
			cw1h = cw
		}
		usage := models.CacheUsage{
			NetInputTokens:        mu.InputTokens,
			OutputTokens:          mu.OutputTokens,
			CacheReadTokens:       mu.CacheReadInputTokens,
			CacheCreationTokens:   cw,
			CacheCreation1hTokens: cw1h,
		}
		if cacheobs.IsZeroUsage(usage) {
			continue
		}
		messageID := "result:" + rec.UUID + ":" + model
		obs := acc.Emit(path, sessionID, messageID, model, ts, usage, false)
		// §15.3 boundary: cowork is Anthropic-first-party (the
		// audit.jsonl usage shape is the Anthropic Messages API
		// response verbatim, including the 5m/1h cache-creation
		// tier split no gateway-fronted provider exposes) — the
		// overlay is applied uniformly for consistency with every
		// other Tier-2 producer, but ApplyImplicitCacheOverlay
		// defaults to false (Anthropic-shape) for cowork's model
		// strings, so this is a no-op in practice.
		obs = cacheobs.ApplyImplicitCacheOverlay(obs, "", model)
		out = append(out, obs)
	}
	return out
}

// accumulateCacheBlocksTier2 converts cowork's Anthropic-shape
// content blocks into deterministic CacheBlockMeta entries. cowork's
// rawContentBlock schema is the same text/thinking/tool_use/
// tool_result/image shape cline and cline-cli already marshal; the
// "image" case carries no payload (image bytes are never stored) —
// only the block's presence/ordering feeds the chain.
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
// canonical form of one cowork content block. Returns (canonical
// bytes, engine kind label, ok=false on unknown block type). Each
// payload is an explicit STRUCT (never a map) so encoding/json's
// deterministic field order preserves the R3 byte-stability
// invariant.
func marshalCanonicalBlockTier2(b rawContentBlock) (canon []byte, kind string, ok bool) {
	switch b.Type {
	case "text":
		return marshalTextLikeBlockTier2("text", b.Text)
	case "thinking":
		return marshalTextLikeBlockTier2("thinking", b.Thinking)
	case "tool_use":
		return marshalToolUseBlockTier2(b)
	case "tool_result":
		return marshalToolResultBlockTier2(b)
	case "image":
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

func marshalTextLikeBlockTier2(kind, text string) (canon []byte, kindOut string, ok bool) {
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

func marshalToolUseBlockTier2(b rawContentBlock) (canon []byte, kind string, ok bool) {
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

func marshalToolResultBlockTier2(b rawContentBlock) (canon []byte, kind string, ok bool) {
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
