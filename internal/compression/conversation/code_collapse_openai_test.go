package conversation

import (
	"context"
	"strings"
	"testing"
)

// TestOpenAIChatCollapse_Fires proves the aggressive code-collapse hook is
// wired into the Chat Completions per-type pre-pass: a code tool_result with
// an exact-span symbol is collapsed to a declaration + retrieval marker, and
// it fires INDEPENDENT of the compress_types allow-list (empty allow here).
func TestOpenAIChatCollapse_Fires(t *testing.T) {
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSym(true)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)
	collapse := p.buildCodeCollapser(context.Background())
	if collapse == nil {
		t.Fatal("collapser nil")
	}
	extracted := []openaiExtractedMessage{{
		isToolResult:    true,
		toolName:        "read_file",
		filename:        "/proj/x.go",
		compressionText: collapseSample,
	}}
	allow := buildAllow(nil) // empty allow-list — collapse must still fire
	events := compressOpenAIChatToolResults(extracted, NewRegistry(), allow, nil, collapse)

	if !strings.Contains(extracted[0].compressedText, "mcp__observer__retrieve_stashed") {
		t.Fatalf("Chat tool_result not collapsed; got %q", extracted[0].compressedText)
	}
	if !hasMechanism(events, "code_collapse") {
		t.Fatalf("no code_collapse event; events=%+v", events)
	}
}

// TestOpenAIResponsesCollapse_Fires is the Responses-API (codex) analog.
func TestOpenAIResponsesCollapse_Fires(t *testing.T) {
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSym(true)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)
	collapse := p.buildCodeCollapser(context.Background())

	extracted := []openaiResponsesExtractedMessage{{
		isToolResult:    true,
		toolName:        "read_file",
		filename:        "/proj/x.go",
		compressionText: collapseSample,
	}}
	allow := buildAllow(nil)
	events := compressOpenAIResponsesToolResults(extracted, NewRegistry(), allow, nil, nil, collapse)

	if !strings.Contains(extracted[0].compressedText, "mcp__observer__retrieve_stashed") {
		t.Fatalf("Responses tool_result not collapsed; got %q", extracted[0].compressedText)
	}
	if !hasMechanism(events, "code_collapse") {
		t.Fatalf("no code_collapse event; events=%+v", events)
	}
}

// TestOpenAICollapse_RefusesHeuristicSpan is the ADR-0005 guard on the OpenAI
// path: a heuristic (Exact=false) span is never collapsed, and per-type
// compression is gated by the (empty) allow-list, so the body is unchanged.
func TestOpenAICollapse_RefusesHeuristicSpan(t *testing.T) {
	lookup := fakeLookup{available: true, syms: []CompressorSymbol{bigSym(false)}}
	p := newCollapsePipeline(t, lookup, map[string]bool{".go": true}, false)
	collapse := p.buildCodeCollapser(context.Background())

	extracted := []openaiExtractedMessage{{
		isToolResult:    true,
		toolName:        "read_file",
		filename:        "/proj/x.go",
		compressionText: collapseSample,
	}}
	compressOpenAIChatToolResults(extracted, NewRegistry(), buildAllow(nil), nil, collapse)
	if extracted[0].compressedText != "" {
		t.Fatalf("heuristic span collapsed — ADR-0005 violation; got %q", extracted[0].compressedText)
	}
}

func hasMechanism(events []Event, mech string) bool {
	for _, e := range events {
		if e.Mechanism == mech {
			return true
		}
	}
	return false
}
