package openclaw

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

// This file wires the MESSAGE-LOG half of OpenClaw's dual-log capture
// (see adapter.go's parseSessionJSONL/parseMessageLine) into cacheobs.
// The message log carries real content for every call it covers (user
// prompts, assistant text, tool calls, tool results) plus that call's
// own usage row, which is exactly what a Tier-2 cache observation needs.
//
// The TRAJECTORY log (parseTrajectoryJSONL) is deliberately left
// unwired here: model.completed events carry no message content at all
// (see trajLine's doc comment) — only data.promptCache.lastCallUsage —
// so there is nothing to build a BlockHashes chain from, and the task's
// rule against fabricating observations rules out synthesizing one. The
// trajectory's sole remaining job (covering gateway-injected turns the
// message log zeroes out, per hasUsage) stays a TokenEvent-only gap;
// wiring it would also require sharing one accumulator's running state
// across two independently-parsed files, which risks incoherent chain
// semantics if the two files are re-parsed out of order. See
// docs/plans/adapter-parity-audit-2026-08-25.md §2.6 for the fuller
// writeup.

// accumulateUserTextCache feeds a user message's (already scrubbed)
// visible text into acc.
func accumulateUserTextCache(acc *cacheobs.Accumulator, text string) {
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

// accumulateAssistantTextCache feeds an assistant message's (already
// scrubbed) visible text part into acc.
func accumulateAssistantTextCache(acc *cacheobs.Accumulator, text string) {
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

// accumulateToolCallCache feeds one toolCall content part's name + raw
// (already scrubbed) arguments JSON into acc.
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

// accumulateToolResultCache feeds one toolResult message's (already
// scrubbed) flattened output text into acc.
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
// a plain text block. Each payload is an explicit STRUCT (never a map)
// so encoding/json's deterministic field order preserves the R3
// byte-stability invariant.
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
// CacheTurnObservation for a message-log usage row, when u carries
// anything worth persisting. Returns nil for the all-zero case (the
// caller already gates on hasUsage before calling this, so this is
// defense in depth, matching every other Tier-2 producer's identical
// gate).
func emitCacheObservation(acc *cacheobs.Accumulator, path, sessionID, messageID, model string, ts time.Time, u tokenUsage) *models.CacheTurnObservation {
	usage := models.CacheUsage{
		NetInputTokens:      u.Input,
		OutputTokens:        u.Output,
		CacheReadTokens:     u.CacheRead,
		CacheCreationTokens: u.CacheWrite,
	}
	if cacheobs.IsZeroUsage(usage) {
		return nil
	}
	obs := acc.Emit(path, sessionID, messageID, model, ts, usage, false)
	// §15.3 boundary: the message log's per-call provider/modelId live on
	// the outer jsonlLine, not the usage row itself, and this helper only
	// sees the usage — provider is always "" here, a no-op today, kept
	// for consistency with every other Tier-2 producer's identical
	// overlay call.
	obs = cacheobs.ApplyImplicitCacheOverlay(obs, "", model)
	return &obs
}
