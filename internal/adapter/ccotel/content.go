package ccotel

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
)

// Content kinds for a ContentRecord.
const (
	KindPrompt     = "prompt"
	KindToolInput  = "tool_input"
	KindToolOutput = "tool_output"
	KindRawBody    = "raw_body"
)

// ContentRecord is one captured content body from the OTel stream (a user
// prompt, tool input/output, or raw API body). Content is the RAW text as
// emitted — the caller scrubs it for secrets and computes the hash before
// storage. Records are produced only for content events Claude Code emits when
// the admin enabled the OTEL_LOG_* flags; an install without them yields none.
type ContentRecord struct {
	RequestID string
	SessionID string
	ToolUseID string
	Kind      string
	Content   string
	// TimeUnixNano is the record's emit time (0 when unset); the caller maps it
	// to a timestamp.
	TimeUnixNano uint64
}

// contentEvents maps recognized event names to the content kind + the candidate
// attribute keys that may carry the body. The Body field is always tried as a
// fallback. Names are matched against LogRecord.EventName and the legacy
// "event.name" attribute, with and without the "claude_code." prefix.
var contentEvents = []struct {
	names []string
	kind  string
	attrs []string
}{
	{[]string{"claude_code.user_prompt", "user_prompt"}, KindPrompt, []string{"prompt", "user_prompt", "content"}},
	{[]string{"claude_code.tool_result", "tool_result"}, KindToolOutput, []string{"tool_output", "output", "result", "content"}},
	{[]string{"claude_code.tool_decision", "tool_decision"}, KindToolInput, []string{"tool_input", "input"}},
}

// ParseContent extracts content bodies from an OTLP logs export. Resources
// tagged as Observer-emitted are skipped (echo guard). Records without any
// content text are dropped. The caller gates this on
// [ingest.otel].content_capture and scrubs/hashes each Content before storage.
func ParseContent(req *collogspb.ExportLogsServiceRequest) []ContentRecord {
	if req == nil {
		return nil
	}
	var out []ContentRecord
	for _, rl := range req.GetResourceLogs() {
		res := attrMap(rl.GetResource().GetAttributes())
		if str(res, attrEmittedBy) == emittedBySelf {
			continue
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				if rc, ok := contentFromRecord(rec, res); ok {
					out = append(out, rc)
				}
			}
		}
	}
	return out
}

// contentFromRecord matches a log record against the content-event table and
// extracts its body. ok is false when the record is not a content event or
// carries no text.
func contentFromRecord(rec *logspb.LogRecord, res map[string]*commonpb.AnyValue) (ContentRecord, bool) {
	name := eventName(rec)
	for _, ce := range contentEvents {
		if !nameMatches(name, ce.names) {
			continue
		}
		recAttrs := attrMap(rec.GetAttributes())
		content := firstStr(recAttrs, res, ce.attrs...)
		if content == "" {
			content = rec.GetBody().GetStringValue()
		}
		if content == "" {
			return ContentRecord{}, false
		}
		return ContentRecord{
			RequestID:    firstStr(recAttrs, res, "request_id", "request.id"),
			SessionID:    firstStr(recAttrs, res, "session.id", "session_id"),
			ToolUseID:    firstStr(recAttrs, res, "tool_use_id", "tool.use_id"),
			Kind:         ce.kind,
			Content:      content,
			TimeUnixNano: rec.GetTimeUnixNano(),
		}, true
	}
	return ContentRecord{}, false
}

// eventName returns the record's event name from the typed field or the legacy
// event.name attribute.
func eventName(rec *logspb.LogRecord) string {
	if n := rec.GetEventName(); n != "" {
		return n
	}
	for _, kv := range rec.GetAttributes() {
		if kv.GetKey() == "event.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// nameMatches reports whether name equals any candidate.
func nameMatches(name string, candidates []string) bool {
	for _, c := range candidates {
		if name == c {
			return true
		}
	}
	return false
}
