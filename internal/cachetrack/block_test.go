package cachetrack

import (
	"bytes"
	"strings"
	"testing"
)

// TestCanonicalizeAnthropic_StripsCacheControl pins the load-
// bearing rule that markers don't change the cached content
// identity (spec §5). Table-driven per CLAUDE.md standard.
func TestCanonicalizeAnthropic_StripsCacheControl(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "text block with marker",
			in:   `{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}`,
			want: `{"text":"hi","type":"text"}`,
		},
		{
			name: "text block without marker (passthrough sort)",
			in:   `{"type":"text","text":"hi"}`,
			want: `{"text":"hi","type":"text"}`,
		},
		{
			name: "tool_use block with marker + 1h ttl",
			in:   `{"type":"tool_use","id":"a","name":"b","input":{"x":1},"cache_control":{"type":"ephemeral","ttl":"1h"}}`,
			want: `{"id":"a","input":{"x":1},"name":"b","type":"tool_use"}`,
		},
		{
			name: "html-unsafe character preserved (no escape)",
			in:   `{"type":"text","text":"a<b>c"}`,
			want: `{"text":"a<b>c","type":"text"}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalizeAnthropic([]byte(tt.in))
			if err != nil {
				t.Fatalf("CanonicalizeAnthropic: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %s\nwant %s", got, tt.want)
			}
		})
	}
}

// TestCanonicalizeAnthropic_Deterministic pins R3 going forward:
// the same input must produce byte-identical output across runs
// (spec §0 Findings R3). A flake here would manifest as false-
// positive mispredicts in §10 reconciliation.
func TestCanonicalizeAnthropic_Deterministic(t *testing.T) {
	t.Parallel()
	in := []byte(`{"z":1,"a":"abc","m":[1,2,3],"cache_control":{"type":"ephemeral"}}`)
	first, err := CanonicalizeAnthropic(in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 10; i++ {
		got, err := CanonicalizeAnthropic(in)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("iter %d: drift\n got %s\n want %s", i, got, first)
		}
	}
}

// TestCanonicalizeAnthropic_Invalid surfaces bad input with a
// wrapped error (CLAUDE.md error-wrap convention).
func TestCanonicalizeAnthropic_Invalid(t *testing.T) {
	t.Parallel()
	_, err := CanonicalizeAnthropic([]byte(`not json`))
	if err == nil {
		t.Fatal("want error for invalid JSON, got nil")
	}
	if !strings.HasPrefix(err.Error(), "cachetrack.CanonicalizeAnthropic:") {
		t.Errorf("want wrapped prefix, got %q", err.Error())
	}
}

// TestCanonicalizeTranscript_StableShape verifies Tier-2
// canonicalization wraps content in a stable role/type/content
// envelope. Table-driven.
func TestCanonicalizeTranscript_StableShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		role    string
		kind    string
		content string
		want    string
	}{
		{
			name:    "user text quoted-json passthrough",
			role:    "user",
			kind:    "text",
			content: `"hi there"`,
			want:    `{"content":"hi there","role":"user","type":"text"}`,
		},
		{
			name:    "raw text content gets quoted",
			role:    "user",
			kind:    "text",
			content: `hi there`,
			want:    `{"content":"hi there","role":"user","type":"text"}`,
		},
		{
			name:    "attachment with structured json content",
			role:    "user",
			kind:    "attachment",
			content: `{"path":"/x","sha":"abc"}`,
			want:    `{"content":{"path":"/x","sha":"abc"},"role":"user","type":"attachment"}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalizeTranscript(tt.role, tt.kind, []byte(tt.content))
			if err != nil {
				t.Fatalf("CanonicalizeTranscript: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %s\nwant %s", got, tt.want)
			}
		})
	}
}

// TestEstimateTokens covers the kind→estimate table.
func TestEstimateTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind  string
		bytes int
		want  int
	}{
		{"text", 0, 0},
		{"text", 1, 1},
		{"text", 3, 1},
		{"text", 4, 1},
		{"text", 5, 2},
		{"text", 16, 4},
		{"tool_use", 100, 25},
		{"tool_result", 1023, 256},
		{"thinking", 7, 2},
		{"attachment", 17, 5},
		{"image", 9, tokensImageFloor},
		{"image", 99999, tokensImageFloor},
		{"document", 9, tokensDocumentFloor},
	}
	for _, tt := range tests {
		got := EstimateTokens(tt.kind, make([]byte, tt.bytes))
		if got != tt.want {
			t.Errorf("EstimateTokens(%q, %d) = %d, want %d", tt.kind, tt.bytes, got, tt.want)
		}
	}
}

// TestBlockLevel_String pins the strings persisted to
// cache_segments.level. Changing them is a schema break.
func TestBlockLevel_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		l    BlockLevel
		want string
	}{
		{LevelUnknown, "unknown"},
		{LevelTools, "tools"},
		{LevelSystem, "system"},
		{LevelMessage, "message"},
		{BlockLevel(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.l.String(); got != tt.want {
			t.Errorf("BlockLevel(%d).String() = %q, want %q", tt.l, got, tt.want)
		}
	}
}

// TestBlockTTL_String pins the strings persisted to
// cache_segments.ttl_tier (empty for Unset → NULL column).
func TestBlockTTL_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ttl  BlockTTL
		want string
	}{
		{TTLUnset, ""},
		{TTL5m, "5m"},
		{TTL1h, "1h"},
		{BlockTTL(99), ""},
	}
	for _, tt := range tests {
		if got := tt.ttl.String(); got != tt.want {
			t.Errorf("BlockTTL(%d).String() = %q, want %q", tt.ttl, got, tt.want)
		}
	}
}
