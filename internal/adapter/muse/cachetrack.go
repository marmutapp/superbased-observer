package muse

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

// accumulatePromptCache feeds a run-level `started` event's (already
// scrubbed) prompt text into acc, as a single "user" text block. A
// `started` event is one-shot per run (see emitRunStart's isSubagent /
// task-id carve-out), so no once-only guard is needed.
func accumulatePromptCache(acc *cacheobs.Accumulator, text string) {
	canon, ok := marshalCacheTextBlock(text)
	if !ok {
		return
	}
	acc.ObserveBlocks([]models.CacheBlockMeta{{
		LevelLabel:     "message", // R1: transcripts never expose tools/system
		Kind:           "text",
		CanonicalBytes: canon,
		Role:           "user",
	}})
}

// accumulateAssistantMessageCache feeds an assistant_message_committed
// event's (already scrubbed) visible reply into acc.
func accumulateAssistantMessageCache(acc *cacheobs.Accumulator, text string) {
	canon, ok := marshalCacheTextBlock(text)
	if !ok {
		return
	}
	acc.ObserveBlocks([]models.CacheBlockMeta{{
		LevelLabel:     "message",
		Kind:           "text",
		CanonicalBytes: canon,
		Role:           "assistant",
	}})
}

// accumulateToolCallCache feeds one assistant_tool_calls_committed entry's
// name + (already scrubbed) serialized args into acc.
func accumulateToolCallCache(acc *cacheobs.Accumulator, name, scrubbedArgs string) {
	canon, ok := marshalCacheToolUseBlock(name, scrubbedArgs)
	if !ok {
		return
	}
	acc.ObserveBlocks([]models.CacheBlockMeta{{
		LevelLabel:     "message",
		Kind:           "tool_use",
		CanonicalBytes: canon,
		Role:           "assistant",
	}})
}

// accumulateToolResultCache feeds one tool_result_batch_committed entry's
// (already scrubbed) result text into acc.
func accumulateToolResultCache(acc *cacheobs.Accumulator, scrubbedText string) {
	canon, ok := marshalCacheToolResultBlock(scrubbedText)
	if !ok {
		return
	}
	acc.ObserveBlocks([]models.CacheBlockMeta{{
		LevelLabel:     "message",
		Kind:           "tool_result",
		CanonicalBytes: canon,
		Role:           "tool",
	}})
}

// marshalCacheTextBlock produces the deterministic JSON canonical form of
// a plain text block (a user prompt or an assistant message). Each
// payload is an explicit STRUCT (never a map) so encoding/json's
// deterministic field order preserves the R3 byte-stability invariant.
func marshalCacheTextBlock(text string) ([]byte, bool) {
	if text == "" {
		return nil, false
	}
	payload := struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}{Type: "text", Text: text}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return buf, true
}

// marshalCacheToolUseBlock produces the canonical form of one assistant
// tool call.
func marshalCacheToolUseBlock(name, args string) ([]byte, bool) {
	if name == "" && args == "" {
		return nil, false
	}
	payload := struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
		Args string `json:"args,omitempty"`
	}{Type: "tool_use", Name: name, Args: args}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return buf, true
}

// marshalCacheToolResultBlock produces the canonical form of one tool
// result's flattened text.
func marshalCacheToolResultBlock(text string) ([]byte, bool) {
	if text == "" {
		return nil, false
	}
	payload := struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}{Type: "tool_result", Text: text}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return buf, true
}

// emitCacheObservation drains acc's pending content-block delta into one
// CacheTurnObservation for a model_completed record's usage, when tp
// carries anything worth persisting. Returns nil when the derived
// CacheUsage is all-zero — emitTokens already gates on e.Usage.isZero()
// before calling this, so tp here is never the zero value in practice,
// but the check is kept for defense in depth (mirrors every other Tier-2
// producer's identical gate).
func emitCacheObservation(acc *cacheobs.Accumulator, path, sessionID, messageID, model string, ts time.Time, tp tokenParts) *models.CacheTurnObservation {
	usage := models.CacheUsage{
		NetInputTokens:      tp.inputNet,
		OutputTokens:        tp.outputNet,
		CacheReadTokens:     tp.cacheRead,
		CacheCreationTokens: tp.cacheWrit,
	}
	if cacheobs.IsZeroUsage(usage) {
		return nil
	}
	obs := acc.Emit(path, sessionID, messageID, model, ts, usage, false)
	// §15.3 boundary: Muse's model_completed record never states which
	// upstream provider served the model (provider_id lives only on the
	// unrelated run.model.configured record we don't thread through
	// here), so provider is always "" — a no-op today, kept for
	// consistency with every other Tier-2 producer's identical overlay
	// call.
	obs = cacheobs.ApplyImplicitCacheOverlay(obs, "", model)
	return &obs
}
