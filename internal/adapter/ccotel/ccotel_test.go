package ccotel

import (
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func sv(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func iv(i int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}
}

func dv(f float64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
}

func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

// export wraps one resource (with attrs) holding the given records.
func export(resAttrs []*commonpb.KeyValue, recs ...*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource:  &resourcepb.Resource{Attributes: resAttrs},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}},
		}},
	}
}

func TestParseLogs_APIRequestEvent(t *testing.T) {
	rec := &logspb.LogRecord{
		EventName:    "claude_code.api_request",
		TimeUnixNano: 1_700_000_000_000_000_000,
		Attributes: []*commonpb.KeyValue{
			kv("request_id", sv("req_abc")),
			kv("model", sv("claude-opus-4-8")),
			kv("input_tokens", iv(1000)),
			kv("output_tokens", iv(200)),
			kv("cache_read_tokens", iv(50)),
			kv("cache_creation_tokens", iv(10)),
			kv("cost_usd", dv(0.42)),
			kv("duration_ms", iv(1500)),
		},
	}
	res := []*commonpb.KeyValue{kv("session.id", sv("sess-1"))}

	turns, skipped := ParseLogs(export(res, rec))
	if skipped != 0 || len(turns) != 1 {
		t.Fatalf("got %d turns, %d skipped; want 1,0", len(turns), skipped)
	}
	got := turns[0]
	if got.RequestID != "req_abc" || got.Model != "claude-opus-4-8" || got.Provider != "anthropic" {
		t.Fatalf("identity wrong: %+v", got)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 200 || got.CacheReadTokens != 50 ||
		got.CacheCreationTokens != 10 || got.CostUSD != 0.42 || got.TotalResponseMS != 1500 {
		t.Fatalf("metrics wrong: %+v", got)
	}
	if got.SessionID != "sess-1" {
		t.Fatalf("session from resource attr not picked up: %q", got.SessionID)
	}
	if got.Source != SourceTag {
		t.Fatalf("source = %q, want %q", got.Source, SourceTag)
	}
	if got.Timestamp.IsZero() {
		t.Fatalf("timestamp not resolved from TimeUnixNano")
	}
}

func TestParseLogs_LegacyEventNameAttributeAndDoubleTokens(t *testing.T) {
	// Older Claude Code: event name in an attribute, counts as doubles,
	// request id under "request.id".
	rec := &logspb.LogRecord{
		Attributes: []*commonpb.KeyValue{
			kv("event.name", sv("api_request")),
			kv("request.id", sv("req_legacy")),
			kv("input_tokens", dv(7)),
		},
	}
	turns, skipped := ParseLogs(export(nil, rec))
	if len(turns) != 1 || skipped != 0 {
		t.Fatalf("got %d turns %d skipped; want 1,0", len(turns), skipped)
	}
	if turns[0].RequestID != "req_legacy" || turns[0].InputTokens != 7 {
		t.Fatalf("legacy/double parse wrong: %+v", turns[0])
	}
}

func TestParseLogs_SkipsNonAPIRequestAndMissingRequestID(t *testing.T) {
	other := &logspb.LogRecord{
		EventName:  "claude_code.tool_result",
		Attributes: []*commonpb.KeyValue{kv("tool_use_id", sv("tu-1"))},
	}
	noReqID := &logspb.LogRecord{
		EventName:  "claude_code.api_request",
		Attributes: []*commonpb.KeyValue{kv("input_tokens", iv(5))},
	}

	turns, skipped := ParseLogs(export(nil, other, noReqID))
	if len(turns) != 0 {
		t.Fatalf("unexpected turns: %+v", turns)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the api_request lacking request_id)", skipped)
	}
}

func TestParseLogs_EchoGuardDropsObserverEmitted(t *testing.T) {
	rec := &logspb.LogRecord{
		EventName:  "claude_code.api_request",
		Attributes: []*commonpb.KeyValue{kv("request_id", sv("req_echo"))},
	}
	res := []*commonpb.KeyValue{kv(attrEmittedBy, sv(emittedBySelf))}

	turns, skipped := ParseLogs(export(res, rec))
	if len(turns) != 0 || skipped != 0 {
		t.Fatalf("echo guard failed: %d turns %d skipped", len(turns), skipped)
	}
}

func TestParseLogs_NilSafe(t *testing.T) {
	if turns, skipped := ParseLogs(nil); turns != nil || skipped != 0 {
		t.Fatalf("nil request not handled: %v %d", turns, skipped)
	}
}
