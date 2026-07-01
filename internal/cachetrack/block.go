package cachetrack

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// BlockLevel labels which level of the Anthropic request hierarchy
// a block sits in. Caches invalidate top-down: a tools-level change
// kills everything after it; a system-level change kills system +
// messages; a message-level change kills only messages. The label
// is part of the chain preimage (chain.go) so two blocks with
// identical canonical bytes at different levels produce different
// chain hashes.
type BlockLevel uint8

// BlockLevel values. The String form is persisted to
// cache_segments.level — stable across releases.
const (
	LevelUnknown BlockLevel = iota
	LevelTools
	LevelSystem
	LevelMessage
)

// String returns the canonical level label persisted to
// cache_segments.level (literal strings — the SQL schema stores
// them verbatim).
func (l BlockLevel) String() string {
	switch l {
	case LevelTools:
		return "tools"
	case LevelSystem:
		return "system"
	case LevelMessage:
		return "message"
	default:
		return "unknown"
	}
}

// BlockLevelFromLabel parses the canonical level label back into
// a BlockLevel — the inverse of String(). Used by the store-side
// Tier-2 path (store.Ingest's cache observations) which carries
// the level as the schema-stable string label rather than the
// cachetrack enum (spec §24.1: adapters MUST NOT import
// cachetrack). Unknown labels return LevelUnknown.
func BlockLevelFromLabel(s string) BlockLevel {
	switch s {
	case "tools":
		return LevelTools
	case "system":
		return LevelSystem
	case "message":
		return LevelMessage
	default:
		return LevelUnknown
	}
}

// BlockTTL labels the ephemeral cache TTL a Tier-1 marker selected.
// Tier-2 inputs leave this Unset; the state machine reads the
// observed usage envelope (ephemeral_1h_input_tokens > 0) instead
// per spec §6 (R1 finding: 1h is the common case for Claude Code,
// not the exception).
type BlockTTL uint8

// BlockTTL values. String returns "" when Unset so the
// cache_segments.ttl_tier column lands NULL.
const (
	TTLUnset BlockTTL = iota
	TTL5m
	TTL1h
)

// String returns the canonical TTL label persisted to
// cache_segments.ttl_tier (empty when Unset).
func (t BlockTTL) String() string {
	switch t {
	case TTL5m:
		return "5m"
	case TTL1h:
		return "1h"
	default:
		return ""
	}
}

// Block is one element of the per-turn cache prefix chain.
//
// CanonicalBytes is the SHA-256 chain input: the block's JSON body
// with cache_control fields stripped (see CanonicalizeAnthropic /
// CanonicalizeTranscript). The Level + Kind labels carry tier-
// agnostic provenance for §7 attribution and dashboard rendering;
// they ALSO appear in the chain preimage so two blocks with
// identical canonical bytes but different level/kind produce
// different chain hashes (defense against rare cross-level
// content collisions).
//
// MarkerPresent + TTL are Tier-1-only signals; Tier-2 callers
// leave them zero.
type Block struct {
	Level          BlockLevel
	Kind           string // text | tool_use | tool_result | image | thinking | document | attachment
	CanonicalBytes []byte
	TokenEstimate  int
	MarkerPresent  bool
	TTL            BlockTTL
}

// CanonicalizeAnthropic returns the canonical hash bytes of a raw
// Anthropic block JSON.
//
// Deep-deterministic via interface{} round-trip: encoding/json
// sorts map[string]any keys at EVERY depth on Marshal, so the
// SAME logical content with a different nested key order produces
// the SAME canonical bytes. (The shallow predecessor sorted only
// top-level keys, then preserved nested objects byte-verbatim;
// see canonical_equivalence_test.go for the equivalence probe
// that documents the failure mode.)
//
// cache_control is a MARKER (it selects which prefix to cache);
// it is NOT part of the cached content identity — two requests
// with identical content but markers in different positions hit
// the same cache. Stripping it from the chain preimage is the
// correct shape per spec §5. The strip is RECURSIVE (every
// depth) so a marker nested inside a tool's input_schema or
// content sub-block can't leak into the hash.
//
// The input is a SINGLE block (one tools entry, one system text
// block, or one message content block). Callers that hold an
// array iterate per-block; the chain is built one block at a
// time.
func CanonicalizeAnthropic(raw []byte) ([]byte, error) {
	v, err := decodeJSONWithNumber(raw)
	if err != nil {
		return nil, fmt.Errorf("cachetrack.CanonicalizeAnthropic: %w", err)
	}
	v = stripCacheControlDeep(v)
	out, err := canonicalizeDeepValue(v)
	if err != nil {
		return nil, fmt.Errorf("cachetrack.CanonicalizeAnthropic: %w", err)
	}
	return out, nil
}

// CanonicalizeTranscript returns the canonical hash bytes of a
// Tier-2 transcript block (role + type + content), JSON-serialized
// in a stable shape (deeply-sorted keys, no HTML escape). Tier-2
// chains are stable against themselves but NOT comparable to
// Tier-1 hashes — never mix chains across tiers within a session.
// The dedup gate at spec §4 design notes / §9 enforces this at the
// engine level.
//
// content is parsed when valid JSON (so nested keys land in the
// shared deep-canonicalize path with everything else); bare text
// content is wrapped as a string so the envelope shape stays
// JSON-shaped. Tier-2 transcripts don't carry Anthropic
// cache_control markers (they're observer-side reconstruction);
// no strip is needed here.
//
// §24.4 single-canonicalizer: shares canonicalizeDeepValue with
// CanonicalizeAnthropic so a nested-order drift caught and fixed
// in one tier can never resurface in the other.
func CanonicalizeTranscript(role, kind string, content []byte) ([]byte, error) {
	var contentVal any
	if json.Valid(content) {
		v, err := decodeJSONWithNumber(content)
		if err != nil {
			return nil, fmt.Errorf("cachetrack.CanonicalizeTranscript: %w", err)
		}
		contentVal = v
	} else {
		contentVal = string(content)
	}
	envelope := map[string]any{
		"role":    role,
		"type":    kind,
		"content": contentVal,
	}
	out, err := canonicalizeDeepValue(envelope)
	if err != nil {
		return nil, fmt.Errorf("cachetrack.CanonicalizeTranscript: %w", err)
	}
	return out, nil
}

// canonicalizeDeepValue marshals v with deep-deterministic key
// ordering. encoding/json sorts map[string]any keys at EVERY
// depth on Marshal — that's the source of the deep-canonical
// property both CanonicalizeAnthropic and CanonicalizeTranscript
// rely on. SetEscapeHTML(false) preserves verbatim `<`/`>`/`&` so
// JSON-shaped content with HTML-unsafe characters is identity-
// preserved.
//
// This helper is the ONLY canonical-bytes-emitting path on the
// hash side (§24.4 single-canonicalizer). Per-tier pre-processing
// (cache_control strip, envelope wrap) happens in the public
// CanonicalizeXxx entry points BEFORE this is called.
func canonicalizeDeepValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// decodeJSONWithNumber decodes raw into an interface{} value
// preserving numeric source form via json.UseNumber. Without
// UseNumber, encoding/json's default decodes numbers as float64,
// which (a) mangles ints > 2^53 (silent precision loss) and (b)
// silently collapses `1.0` → `1` (the float-to-int normalization
// path) — both would introduce their OWN canonical-bytes drift
// on legitimate cross-turn variation and defeat the determinism
// guarantee the deep round-trip is meant to give.
func decodeJSONWithNumber(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// stripCacheControlDeep recursively removes cache_control fields
// at every depth of v. Anthropic's cache_control field is the
// per-block cache-position MARKER and never part of the cached
// CONTENT identity (spec §5 + R1(a)); a marker nested inside a
// tool's input_schema or any content sub-block is equally
// identity-neutral and must not leak into the hash.
//
// In-place mutation of the input map/slice is fine — the caller
// owns v (it came from decodeJSONWithNumber) and discards it
// after the marshal.
func stripCacheControlDeep(v any) any {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "cache_control")
		for k, vv := range x {
			x[k] = stripCacheControlDeep(vv)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = stripCacheControlDeep(vv)
		}
		return x
	default:
		return v
	}
}

// EstimateTokens returns the v1 token estimate for a block of the
// given kind. R6 calibration during C8 will refine these constants
// from observed (canonical_bytes, provider_input_tokens) tuples;
// the per-session EMA in reconcile.go (§10) layers on top.
//
// Floors for non-text kinds reflect the Anthropic Messages API's
// per-image/document overhead (~1500 tokens) plus a token-count
// fast-path that avoids parsing the inner image payload. These are
// STARTING values; the call shape stays even after R6 updates the
// constants.
func EstimateTokens(kind string, canonical []byte) int {
	switch kind {
	case "image":
		return tokensImageFloor
	case "document":
		return tokensDocumentFloor
	default:
		// Text + tool_use + tool_result + thinking + attachment all
		// scale roughly with byte count (inner bodies are strings).
		// ceil(len/4) is the standard provider tokens-per-byte
		// heuristic; R6 logs the observed scale factor.
		n := len(canonical)
		if n == 0 {
			return 0
		}
		return (n + 3) / 4
	}
}

// tokensImageFloor and tokensDocumentFloor are R6-placeholder
// per-kind token estimates. They are intentionally undermined
// (smaller than worst-case provider overhead) so reconciliation
// catches under-estimates as positive scale factors, never
// negative. Update during C8 from live calibration.
const (
	tokensImageFloor    = 1500
	tokensDocumentFloor = 1500
)
