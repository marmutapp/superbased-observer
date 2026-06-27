package ccotel

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// SourceTag is the provenance value stamped on turns parsed here. It MUST equal
// store.SourceCCOTel — the store's source<->fidelity boundary maps it to
// FidelityNativeExact. Kept as a literal to avoid an adapter->store import; a
// guard test pins the equality.
const SourceTag = "cc_otel"

// resourceEmittedByObserver is the resource attribute Observer stamps on spans
// it emits itself (internal/exporter/otel). The receiver drops these on the way
// in (§3.2 echo guard); ParseLogs honors the same guard for logs.
const (
	attrEmittedBy = "sbo.emitted_by"
	emittedBySelf = "observer"
)

// apiRequestEventNames are the event names Claude Code has used for the
// per-turn API-request log record across versions. Matched against both
// LogRecord.EventName and the legacy "event.name" attribute.
var apiRequestEventNames = map[string]bool{
	"claude_code.api_request": true,
	"api_request":             true,
}

// ParseLogs maps an OTLP logs export to API-turn observations. It returns the
// recognized api_request turns and skipped — the count of api_request events
// dropped because they carried no request_id (which would risk duplication
// against a proxied row, so they are not emitted). Resources tagged as
// Observer-emitted are ignored (echo guard).
func ParseLogs(req *collogspb.ExportLogsServiceRequest) (turns []models.APITurn, skipped int) {
	if req == nil {
		return nil, 0
	}
	for _, rl := range req.GetResourceLogs() {
		res := attrMap(rl.GetResource().GetAttributes())
		if str(res, attrEmittedBy) == emittedBySelf {
			continue // echo guard — never re-ingest our own emitted telemetry
		}
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				if !isAPIRequest(rec) {
					continue
				}
				turn, ok := turnFromRecord(rec, res)
				if !ok {
					skipped++
					continue
				}
				turns = append(turns, turn)
			}
		}
	}
	return turns, skipped
}

// isAPIRequest reports whether a log record is the Claude Code per-turn
// api_request event, checking the typed EventName field then the legacy
// event.name attribute.
func isAPIRequest(rec *logspb.LogRecord) bool {
	if apiRequestEventNames[rec.GetEventName()] {
		return true
	}
	for _, kv := range rec.GetAttributes() {
		if kv.GetKey() == "event.name" {
			return apiRequestEventNames[kv.GetValue().GetStringValue()]
		}
	}
	return false
}

// turnFromRecord maps an api_request record (with its resource attributes as
// fallback) to a models.APITurn. ok is false when no request_id is present.
func turnFromRecord(rec *logspb.LogRecord, res map[string]*commonpb.AnyValue) (models.APITurn, bool) {
	recAttrs := attrMap(rec.GetAttributes())
	requestID := firstStr(recAttrs, res, "request_id", "request.id")
	if requestID == "" {
		return models.APITurn{}, false
	}
	t := models.APITurn{
		Provider:            "anthropic", // Claude Code is always Anthropic upstream
		Model:               firstStr(recAttrs, res, "model"),
		RequestID:           requestID,
		SessionID:           firstStr(recAttrs, res, "session.id", "session_id"),
		Timestamp:           recordTime(rec),
		InputTokens:         firstInt(recAttrs, res, "input_tokens"),
		OutputTokens:        firstInt(recAttrs, res, "output_tokens"),
		CacheReadTokens:     firstInt(recAttrs, res, "cache_read_tokens"),
		CacheCreationTokens: firstInt(recAttrs, res, "cache_creation_tokens"),
		CostUSD:             firstFloat(recAttrs, res, "cost_usd"),
		TotalResponseMS:     firstInt(recAttrs, res, "duration_ms"),
		Source:              SourceTag,
	}
	return t, true
}

// recordTime resolves a log record's timestamp, preferring the emit time and
// falling back to the observed time; zero when neither is set (the caller's
// store insert tolerates a zero timestamp).
func recordTime(rec *logspb.LogRecord) time.Time {
	if ns := rec.GetTimeUnixNano(); ns != 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	if ns := rec.GetObservedTimeUnixNano(); ns != 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	return time.Time{}
}

// attrMap indexes an attribute slice by key for O(1) lookup.
func attrMap(kvs []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	m := make(map[string]*commonpb.AnyValue, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = kv.GetValue()
	}
	return m
}

// firstStr / firstInt / firstFloat return the first present value across the
// record attributes then the resource attributes, trying each candidate key.
func firstStr(rec, res map[string]*commonpb.AnyValue, keys ...string) string {
	for _, k := range keys {
		if v := str(rec, k); v != "" {
			return v
		}
		if v := str(res, k); v != "" {
			return v
		}
	}
	return ""
}

func firstInt(rec, res map[string]*commonpb.AnyValue, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := intVal(rec, k); ok {
			return v
		}
		if v, ok := intVal(res, k); ok {
			return v
		}
	}
	return 0
}

func firstFloat(rec, res map[string]*commonpb.AnyValue, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := floatVal(rec, k); ok {
			return v
		}
		if v, ok := floatVal(res, k); ok {
			return v
		}
	}
	return 0
}

// str reads a string attribute, returning "" when absent.
func str(m map[string]*commonpb.AnyValue, key string) string {
	if v, ok := m[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

// intVal reads an integer attribute, tolerating a numeric value delivered as a
// double (some exporters encode counts as doubles). ok is false when absent.
func intVal(m map[string]*commonpb.AnyValue, key string) (int64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch v.GetValue().(type) {
	case *commonpb.AnyValue_IntValue:
		return v.GetIntValue(), true
	case *commonpb.AnyValue_DoubleValue:
		return int64(v.GetDoubleValue()), true
	default:
		return 0, false
	}
}

// floatVal reads a double attribute, tolerating an integer value. ok is false
// when absent.
func floatVal(m map[string]*commonpb.AnyValue, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch v.GetValue().(type) {
	case *commonpb.AnyValue_DoubleValue:
		return v.GetDoubleValue(), true
	case *commonpb.AnyValue_IntValue:
		return float64(v.GetIntValue()), true
	default:
		return 0, false
	}
}
