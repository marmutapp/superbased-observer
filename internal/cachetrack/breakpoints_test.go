package cachetrack

import (
	"encoding/json"
	"testing"
)

// TestEnumerateAnthropicBlocks_R1aShape pins the live R1(a)
// capture shape: 3 of 4 max breakpoints — 2 in the system array
// + 1 rolling marker on the last message block — all 1h tier.
// Plus zero tools markers (tools blocks cached under the first
// system breakpoint via the hierarchy).
func TestEnumerateAnthropicBlocks_R1aShape(t *testing.T) {
	t.Parallel()
	tools := []json.RawMessage{
		json.RawMessage(`{"name":"Read","description":"read","input_schema":{"type":"object"}}`),
		json.RawMessage(`{"name":"Write","description":"write","input_schema":{"type":"object"}}`),
	}
	system := json.RawMessage(`[
		{"type":"text","text":"You are Claude Code."},
		{"type":"text","text":"system block 1 (project tools)","cache_control":{"type":"ephemeral","ttl":"1h"}},
		{"type":"text","text":"system block 2 (codebase rules)","cache_control":{"type":"ephemeral","ttl":"1h"}}
	]`)
	messages := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[
			{"type":"text","text":"hello"},
			{"type":"text","text":"do something","cache_control":{"type":"ephemeral","ttl":"1h"}}
		]}`),
	}

	blocks, breakpoints := EnumerateAnthropicBlocks(tools, system, messages)

	if got, want := len(blocks), 2+3+2; got != want {
		t.Errorf("block count = %d, want %d (2 tools + 3 system + 2 message)", got, want)
	}
	// Order: tools[0], tools[1], system[0], system[1], system[2],
	//        message blocks 0, 1.
	wantLevels := []BlockLevel{
		LevelTools, LevelTools,
		LevelSystem, LevelSystem, LevelSystem,
		LevelMessage, LevelMessage,
	}
	for i, w := range wantLevels {
		if blocks[i].Level != w {
			t.Errorf("block %d level = %v, want %v", i, blocks[i].Level, w)
		}
	}

	if got := len(breakpoints); got != 3 {
		t.Fatalf("breakpoints = %d, want 3 (R1(a) shape: 2 system + 1 rolling message)", got)
	}
	// Breakpoints at: system[1] (block index 3), system[2]
	// (block index 4), message-content[1] (block index 6).
	wantBPs := []ObserveBreakpoint{
		{BlockIndex: 3, Level: LevelSystem, TTL: TTL1h},
		{BlockIndex: 4, Level: LevelSystem, TTL: TTL1h},
		{BlockIndex: 6, Level: LevelMessage, TTL: TTL1h},
	}
	for i, w := range wantBPs {
		if breakpoints[i] != w {
			t.Errorf("breakpoint %d = %+v, want %+v", i, breakpoints[i], w)
		}
	}
}

// TestEnumerateAnthropicBlocks_NoMarkers covers a cache-cold
// request. Blocks land but no breakpoints.
func TestEnumerateAnthropicBlocks_NoMarkers(t *testing.T) {
	t.Parallel()
	system := json.RawMessage(`"You are helpful."`)
	messages := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
	}
	blocks, breakpoints := EnumerateAnthropicBlocks(nil, system, messages)
	if len(blocks) != 2 {
		t.Errorf("blocks = %d, want 2 (1 system + 1 message)", len(blocks))
	}
	if len(breakpoints) != 0 {
		t.Errorf("breakpoints = %d, want 0 on a cache-cold request", len(breakpoints))
	}
}

// TestEnumerateAnthropicBlocks_5mMarker confirms 5m tier detection
// when the marker explicitly says "5m" or omits ttl.
func TestEnumerateAnthropicBlocks_5mMarker(t *testing.T) {
	t.Parallel()
	system := json.RawMessage(`[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]`)
	_, breakpoints := EnumerateAnthropicBlocks(nil, system, nil)
	if len(breakpoints) != 1 {
		t.Fatalf("breakpoints = %d, want 1", len(breakpoints))
	}
	if breakpoints[0].TTL != TTL5m {
		t.Errorf("TTL = %v, want TTL5m (ephemeral without ttl field defaults to 5m)", breakpoints[0].TTL)
	}
}

// TestEnumerateAnthropicBlocks_ToolMarkerLands covers the case where
// a tool definition carries cache_control (rare but spec-valid).
func TestEnumerateAnthropicBlocks_ToolMarkerLands(t *testing.T) {
	t.Parallel()
	tools := []json.RawMessage{
		json.RawMessage(`{"name":"X","cache_control":{"type":"ephemeral","ttl":"1h"}}`),
	}
	blocks, breakpoints := EnumerateAnthropicBlocks(tools, nil, nil)
	if len(blocks) != 1 || blocks[0].Level != LevelTools {
		t.Fatalf("blocks = %+v", blocks)
	}
	if len(breakpoints) != 1 || breakpoints[0].Level != LevelTools || breakpoints[0].TTL != TTL1h {
		t.Errorf("breakpoint = %+v, want one at LevelTools 1h", breakpoints)
	}
}

// TestCanonicalizeAndDetectMarker pins the strip-and-detect
// helper: marker presence + TTL extraction + canonical bytes
// match what CanonicalizeAnthropic produces standalone.
func TestCanonicalizeAndDetectMarker(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}`)
	canon, marker, ttl := canonicalizeAndDetectMarker(raw)
	if !marker {
		t.Error("marker should be detected")
	}
	if ttl != TTL1h {
		t.Errorf("TTL = %v, want TTL1h", ttl)
	}
	wantCanon, _ := CanonicalizeAnthropic(raw)
	if string(canon) != string(wantCanon) {
		t.Errorf("canonical bytes mismatch:\n got:  %s\n want: %s", canon, wantCanon)
	}
}
