package ccotel

import (
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

func TestParseContent_PromptAndTool(t *testing.T) {
	prompt := &logspb.LogRecord{
		EventName: "claude_code.user_prompt",
		Attributes: []*commonpb.KeyValue{
			kv("request_id", sv("req_1")),
			kv("session.id", sv("sess_1")),
			kv("prompt", sv("refactor the parser")),
		},
	}
	tool := &logspb.LogRecord{
		EventName: "tool_result", // legacy short name
		Attributes: []*commonpb.KeyValue{
			kv("request_id", sv("req_1")),
			kv("tool_use_id", sv("toolu_9")),
			kv("output", sv("3 files changed")),
		},
	}

	recs := ParseContent(export(nil, prompt, tool))
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}
	if recs[0].Kind != KindPrompt || recs[0].Content != "refactor the parser" || recs[0].RequestID != "req_1" {
		t.Fatalf("prompt record wrong: %+v", recs[0])
	}
	if recs[1].Kind != KindToolOutput || recs[1].ToolUseID != "toolu_9" || recs[1].Content != "3 files changed" {
		t.Fatalf("tool record wrong: %+v", recs[1])
	}
}

func TestParseContent_BodyFallbackAndEmptySkipped(t *testing.T) {
	viaBody := &logspb.LogRecord{
		EventName: "user_prompt",
		Body:      &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello from body"}},
		Attributes: []*commonpb.KeyValue{
			kv("request_id", sv("req_b")),
		},
	}
	empty := &logspb.LogRecord{EventName: "user_prompt"} // no content anywhere
	notContent := &logspb.LogRecord{
		EventName:  "claude_code.api_request",
		Attributes: []*commonpb.KeyValue{kv("request_id", sv("x"))},
	}

	recs := ParseContent(export(nil, viaBody, empty, notContent))
	if len(recs) != 1 || recs[0].Content != "hello from body" {
		t.Fatalf("body fallback / empty-skip wrong: %+v", recs)
	}
}

func TestParseContent_EchoGuard(t *testing.T) {
	rec := &logspb.LogRecord{
		EventName:  "user_prompt",
		Attributes: []*commonpb.KeyValue{kv("prompt", sv("x"))},
	}
	res := []*commonpb.KeyValue{kv(attrEmittedBy, sv(emittedBySelf))}
	if recs := ParseContent(export(res, rec)); len(recs) != 0 {
		t.Fatalf("echo guard failed: %+v", recs)
	}
}
