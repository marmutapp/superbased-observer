package cachetrack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// TestBillingHeaderExcluded_SystemHashStableAcrossNonces is the
// load-bearing regression guard for the root cause of the
// 39326aa9 / f81cec3f / 386ff48f soaks: Claude Code 2.1.169+
// prepends a per-request `x-anthropic-billing-header: ... cch=<NONCE>;`
// system text block whose nonce changes every request. Anthropic
// excludes it from prefix-cache identity (provider hit cache on
// every warm turn during the soaks despite the nonce churn), so
// our chain MUST exclude it too — otherwise §7 row 5 fires a
// spurious system_changed → invalidation_rewrite on every healthy
// warm turn, mislabeling 198/201 events at f81cec3f and 13/15 at
// 39326aa9.
//
// The test mirrors a real Claude Code request shape: 3 system
// blocks where ONLY the billing block's cch= nonce differs across
// two turns. The system-level cumulative hash (sha256 over all
// the level's canonical bytes, in chain order) must be byte-
// identical between the turns — proving the billing block was
// excluded.
func TestBillingHeaderExcluded_SystemHashStableAcrossNonces(t *testing.T) {
	t.Parallel()

	// Each turn has the same {tools=[1 tool], system=[billing, stable_intro, stable_main], messages=[1]}
	// shape — only the cch= nonce differs.
	turnA := buildClaudeCodeBodyWithBillingNonce("ce1ac")
	turnB := buildClaudeCodeBodyWithBillingNonce("5e4c4")

	rawA := parseTopLevelForTest(t, turnA)
	rawB := parseTopLevelForTest(t, turnB)

	blocksA, _ := EnumerateAnthropicBlocks(rawA.Tools, rawA.System, rawA.Messages)
	blocksB, _ := EnumerateAnthropicBlocks(rawB.Tools, rawB.System, rawB.Messages)

	// The billing block (system[0]) must be excluded — system[]
	// in the result should contain ONLY the two stable blocks.
	systemA := systemBlocksOf(blocksA)
	systemB := systemBlocksOf(blocksB)
	if len(systemA) != 2 {
		t.Fatalf("turn A: got %d system blocks, want 2 (billing-header block must be skipped); blocks=%+v",
			len(systemA), systemA)
	}
	if len(systemB) != 2 {
		t.Fatalf("turn B: got %d system blocks, want 2; blocks=%+v", len(systemB), systemB)
	}

	// And the system-level cumulative hash must be IDENTICAL
	// across the two turns — the post-fix invariant the soaks
	// need.
	hashA := systemLevelHashHex(systemA)
	hashB := systemLevelHashHex(systemB)
	if hashA != hashB {
		t.Fatalf("system-level hash drifted across turns whose only diff was the billing nonce:\n  A=%s\n  B=%s\n"+
			"  systemA canonical = %s\n  systemB canonical = %s",
			hashA, hashB,
			canonicalSummary(systemA), canonicalSummary(systemB))
	}
}

// TestBillingHeaderExcluded_DoesNotFilterStableSystemBlocks pins
// the opposite case: stable system text blocks with no billing-
// header prefix must NOT be filtered. Otherwise a refactor of the
// filter could silently drop the real system prompt.
func TestBillingHeaderExcluded_DoesNotFilterStableSystemBlocks(t *testing.T) {
	t.Parallel()
	systemArr, _ := json.Marshal([]any{
		map[string]any{"type": "text", "text": "You are a careful assistant."},
		map[string]any{"type": "text", "text": "The user's cwd is /home/marmut."},
	})
	blocks, _ := EnumerateAnthropicBlocks(nil, systemArr, nil)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks; got %d: %+v", len(blocks), blocks)
	}
}

// TestBillingHeaderExcluded_MatchOnPrefixNotExact pins prefix-match
// semantics: the cc_version / cc_entrypoint suffix can change AND
// the cch= nonce is unbounded, so the filter MUST key on the
// "x-anthropic-billing-header:" prefix only, not the full string.
func TestBillingHeaderExcluded_MatchOnPrefixNotExact(t *testing.T) {
	t.Parallel()
	cases := []string{
		"x-anthropic-billing-header: cc_version=2.1.169.715; cc_entrypoint=sdk-cli; cch=abc123;",
		"x-anthropic-billing-header: cc_version=2.2.0.999; cc_entrypoint=desktop; cch=ZZZ;",
		"x-anthropic-billing-header: cc_version=99.0.0.0;", // no cch=
		"x-anthropic-billing-header:",                      // bare prefix
	}
	for _, text := range cases {
		raw, _ := json.Marshal(map[string]any{"type": "text", "text": text})
		if !isProviderExcludedSystemBlock(raw) {
			t.Errorf("did not filter billing-header variant: %q", text)
		}
	}
	// Negative case: only a substring match (mid-string) must not trigger.
	raw, _ := json.Marshal(map[string]any{"type": "text", "text": "PREFIX " + billingHeaderTextPrefix})
	if isProviderExcludedSystemBlock(raw) {
		t.Errorf("filter must require leading prefix, not substring; got match on %q", raw)
	}
	// Non-text type must not be filtered even if text-equivalent.
	raw, _ = json.Marshal(map[string]any{"type": "image", "text": billingHeaderTextPrefix + " whatever"})
	if isProviderExcludedSystemBlock(raw) {
		t.Errorf("filter must only fire on type=text; got match on non-text block")
	}
}

// --- helpers ---

func buildClaudeCodeBodyWithBillingNonce(nonce string) []byte {
	tool, _ := json.Marshal(map[string]any{
		"name":        "Read",
		"description": "Reads a file.",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
			"required":   []string{"file_path"},
		},
	})
	systemArr := []any{
		map[string]any{
			"type": "text",
			"text": "x-anthropic-billing-header: cc_version=2.1.169.715; cc_entrypoint=sdk-cli; cch=" + nonce + ";",
		},
		map[string]any{
			"type":          "text",
			"text":          "You are Claude Code. Follow CLAUDE.md.",
			"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
		},
		map[string]any{
			"type":          "text",
			"text":          "The user's cwd is /home/marmut/superbased-observer.",
			"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
		},
	}
	systemRaw, _ := json.Marshal(systemArr)
	msg, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": "hi"}},
	})
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-8",
		"tools":    []json.RawMessage{tool},
		"system":   json.RawMessage(systemRaw),
		"messages": []json.RawMessage{msg},
	})
	return body
}

func parseTopLevelForTest(t *testing.T, body []byte) struct {
	Tools    []json.RawMessage
	System   json.RawMessage
	Messages []json.RawMessage
} {
	t.Helper()
	var raw struct {
		Tools    []json.RawMessage `json:"tools"`
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return struct {
		Tools    []json.RawMessage
		System   json.RawMessage
		Messages []json.RawMessage
	}{raw.Tools, raw.System, raw.Messages}
}

func systemBlocksOf(blocks []ObserveBlock) []ObserveBlock {
	var out []ObserveBlock
	for _, b := range blocks {
		if b.Level == LevelSystem {
			out = append(out, b)
		}
	}
	return out
}

func systemLevelHashHex(blocks []ObserveBlock) string {
	h := sha256.New()
	for _, b := range blocks {
		h.Write(b.CanonicalBytes)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalSummary(blocks []ObserveBlock) string {
	var s string
	for i, b := range blocks {
		if i > 0 {
			s += " | "
		}
		s += string(b.CanonicalBytes)
	}
	return s
}
