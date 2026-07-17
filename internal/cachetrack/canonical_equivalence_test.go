package cachetrack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// Canonical-equivalence guards for the deep-canonicalize fix
// (the "Fix B root cause" patch). Two logically-equal JSON inputs
// that differ only in nested key order, whitespace, or
// (Anthropic-only) nested cache_control markers MUST produce
// byte-identical canonical output. Without this property the
// chain hash drifts turn-over-turn whenever the upstream
// serializer's nested-key order isn't stable — which is the
// defect the f81cec3f / 386ff48f soaks surfaced (98.5%
// system_changed dominance + read:write 32:1 ratio inconsistent
// with a real invalidation).
//
// Historical note: the predecessor canonicalize was shallow —
// it sorted only top-level keys and preserved nested objects
// byte-verbatim from the source RawMessage. The probe test below
// (now permanent) is the regression guard.

// realToolFixture mirrors the shape of a Claude Code tool def
// (Read), with input_schema carrying multiple keys at multiple
// depths.
const realToolFixture = `{
  "name": "Read",
  "description": "Reads a file from the filesystem.",
  "input_schema": {
    "type": "object",
    "properties": {
      "file_path": {"type": "string", "description": "The absolute path"},
      "offset": {"type": "integer", "description": "Optional start line"},
      "limit": {"type": "integer", "description": "Optional line count"}
    },
    "required": ["file_path"]
  },
  "cache_control": {"type": "ephemeral", "ttl": "1h"}
}`

// realToolFixtureReordered carries the same logical content as
// realToolFixture but with nested map keys in different order
// (and a different required-array placement). Deep canonicalize
// → identical bytes.
const realToolFixtureReordered = `{
  "name": "Read",
  "description": "Reads a file from the filesystem.",
  "input_schema": {
    "required": ["file_path"],
    "properties": {
      "limit": {"description": "Optional line count", "type": "integer"},
      "offset": {"description": "Optional start line", "type": "integer"},
      "file_path": {"description": "The absolute path", "type": "string"}
    },
    "type": "object"
  },
  "cache_control": {"type": "ephemeral", "ttl": "1h"}
}`

// realToolFixturePretty re-formats realToolFixture with extra
// whitespace — should canonicalize identically (json.Unmarshal
// discards inter-value whitespace).
const realToolFixturePretty = `{
    "name"        : "Read"   ,
    "description" : "Reads a file from the filesystem." ,
    "input_schema": {
        "type"      : "object",
        "properties": {
            "file_path": {"type": "string",  "description": "The absolute path"},
            "offset"   : {"type": "integer", "description": "Optional start line"},
            "limit"    : {"type": "integer", "description": "Optional line count"}
        },
        "required"  : ["file_path"]
    },
    "cache_control": {"type": "ephemeral", "ttl": "1h"}
}`

// realToolFixtureNestedCacheControl carries an extra cache_control
// nested INSIDE the input_schema (paranoia case — the SDK never
// does this today, but the strip must be recursive so any future
// nesting can't leak into the hash).
const realToolFixtureNestedCacheControl = `{
  "name": "Read",
  "description": "Reads a file from the filesystem.",
  "input_schema": {
    "type": "object",
    "properties": {
      "file_path": {"type": "string", "description": "The absolute path", "cache_control": {"type":"ephemeral"}},
      "offset": {"type": "integer", "description": "Optional start line"},
      "limit": {"type": "integer", "description": "Optional line count"}
    },
    "required": ["file_path"],
    "cache_control": {"type":"ephemeral","ttl":"1h"}
  },
  "cache_control": {"type": "ephemeral", "ttl": "1h"}
}`

// TestCanonicalize_R3_Determinism_250 — same input → same output
// across 250 iterations. The post-C7 R3 guard widened from 50 to
// 250 since the bug surface that motivated the deep-canonicalize
// patch lives in nested-key handling that the original 50-iter
// guard didn't probe.
func TestCanonicalize_R3_Determinism_250(t *testing.T) {
	first, err := CanonicalizeAnthropic([]byte(realToolFixture))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 250; i++ {
		got, err := CanonicalizeAnthropic([]byte(realToolFixture))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("iter %d drift!\n  first=%s\n  got  =%s", i, first, got)
		}
	}
	h := sha256.Sum256(first)
	t.Logf("R3 deterministic over 250 iterations; sha=%s", hex.EncodeToString(h[:])[:12])
}

// TestCanonicalize_Equivalence_NestedReorder — the load-bearing
// guard. Two byte-different but logically-identical tool defs
// (input_schema with reordered nested keys) MUST produce
// byte-identical canonical output. The pre-fix shallow code
// FAILS this; the deep round-trip PASSES.
func TestCanonicalize_Equivalence_NestedReorder(t *testing.T) {
	a, err := CanonicalizeAnthropic([]byte(realToolFixture))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalizeAnthropic([]byte(realToolFixtureReordered))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("nested-reorder equivalence FAILED — canonicalize is shallow\n  a=%s\n  b=%s", a, b)
	}
}

// TestCanonicalize_Equivalence_Whitespace — pretty-printed and
// compact forms of the same logical content must canonicalize
// identically. json.Unmarshal discards inter-value whitespace;
// this pins that the behavior survives any future internal
// refactor.
func TestCanonicalize_Equivalence_Whitespace(t *testing.T) {
	a, err := CanonicalizeAnthropic([]byte(realToolFixture))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalizeAnthropic([]byte(realToolFixturePretty))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("whitespace equivalence FAILED\n  a=%s\n  b=%s", a, b)
	}
}

// TestCanonicalize_Equivalence_NestedCacheControl — a tool with
// cache_control markers nested INSIDE the input_schema (and on
// individual property entries) must canonicalize to the SAME
// bytes as the version with only the top-level marker. The strip
// is recursive (spec §5: markers are identity-neutral at every
// depth, not just the top).
func TestCanonicalize_Equivalence_NestedCacheControl(t *testing.T) {
	a, err := CanonicalizeAnthropic([]byte(realToolFixture))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalizeAnthropic([]byte(realToolFixtureNestedCacheControl))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("nested-cache_control equivalence FAILED\n  a=%s\n  b=%s", a, b)
	}
	if strings.Contains(string(a), "cache_control") {
		t.Errorf("canonical output still contains cache_control: %s", a)
	}
}

// TestCanonicalize_NumericFormPreserved — UseNumber means `1` and
// `1.0` and `1e0` stay distinct on the hash side (none collapses
// to a normalized form). This is operator-approved behavior: any
// silent normalization would itself introduce drift if the SDK
// later distinguished the forms.
func TestCanonicalize_NumericFormPreserved(t *testing.T) {
	a, err := CanonicalizeAnthropic([]byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalizeAnthropic([]byte(`{"x":1.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Errorf("UseNumber lost: %q == %q (both should canonicalize verbatim)", a, b)
	}
	// And `1e0` stays in source form rather than getting normalized.
	c, err := CanonicalizeAnthropic([]byte(`{"x":1e0}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != `{"x":1e0}` {
		t.Errorf("UseNumber normalized exponent form: got %q, want {\"x\":1e0}", c)
	}
}

// TestCanonicalize_LargeIntPreserved — UseNumber preserves
// integers above 2^53 (where float64 silently loses precision).
// Anthropic IDs (`msg_*`, `chatcmpl_*`) are strings; this guard
// covers schema fields like default token-limit constants.
func TestCanonicalize_LargeIntPreserved(t *testing.T) {
	const big = `{"limit":9007199254740993}` // 2^53 + 1
	got, err := CanonicalizeAnthropic([]byte(big))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != big {
		t.Errorf("large int collapsed: got %q, want %q", got, big)
	}
}

// TestCanonicalizeTranscript_Equivalence_NestedReorder — the
// Tier-2 path goes through the same shared canonicalizer
// (canonicalizeDeepValue) so a tool_use content block whose
// `input` map has nested keys in different order across turns
// canonicalizes identically. Pins §24.4 single-canonicalizer
// across Tier-1 + Tier-2.
func TestCanonicalizeTranscript_Equivalence_NestedReorder(t *testing.T) {
	contentA := `{"input": {"a": 1, "b": 2, "nested": {"x": "p", "y": "q"}}}`
	contentB := `{"input": {"nested": {"y": "q", "x": "p"}, "b": 2, "a": 1}}`
	a, err := CanonicalizeTranscript("user", "tool_use", []byte(contentA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalizeTranscript("user", "tool_use", []byte(contentB))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("Tier-2 nested-reorder equivalence FAILED\n  a=%s\n  b=%s", a, b)
	}
}

// TestCanonicalize_InterfaceMarshalSortsAllDepths is a sanity pin
// on the encoding/json behavior the deep canonicalize relies on:
// json.Marshal of map[string]any sorts keys at every depth.
// Documented Go behavior, but the canonicalize correctness
// derives from it — pin it here so a stdlib quirk would fire a
// loud test failure rather than silently re-introduce drift.
func TestCanonicalize_InterfaceMarshalSortsAllDepths(t *testing.T) {
	v := map[string]any{
		"z": map[string]any{"b": 1, "a": 2},
		"a": []any{
			map[string]any{"y": 1, "x": 2},
		},
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[{"x":2,"y":1}],"z":{"a":2,"b":1}}`
	if string(out) != want {
		t.Fatalf("encoding/json depth-sort assumption broken\n  got  %s\n  want %s", out, want)
	}
}
