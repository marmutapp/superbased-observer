package shape

import (
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/marmutapp/superbased-observer/internal/provenance"
	"github.com/marmutapp/superbased-observer/internal/selfobs/keys"
	"github.com/marmutapp/superbased-observer/internal/selfobs/run"
)

// Producer-side value bounds (R5-SF2). These are the first ("bounded twice")
// layer; the gateway classify envelope budget is the backstop. Truncation is
// byte-length-based but always on a valid UTF-8 rune boundary (a multi-byte rune
// is never split).
const (
	// maxScalarValueLen caps every emitted string scalar, in bytes.
	maxScalarValueLen = 1024
	// maxArrayElems caps the element count of a bounded string array.
	maxArrayElems = 32
	// maxArrayElemLen caps each array element, in bytes.
	maxArrayElemLen = 256
)

// GenAI + usage attribute keys (OTel gen_ai semantic conventions). Held as local
// consts so this package is not coupled to a specific semconv module version.
const (
	keyGenAIModel      = "gen_ai.request.model"
	keyGenAISystem     = "gen_ai.system"
	keyGenAIInputToks  = "gen_ai.usage.input_tokens"
	keyGenAIOutputToks = "gen_ai.usage.output_tokens"
	keyGenAITotalToks  = "gen_ai.usage.total_tokens"
	// keyToolAccesses / keyDecisions are content-risk bounded arrays; they are
	// NOT in the retained-scalar registry (hashed at L0 unless operator-opted-in).
	keyToolAccesses = "sbo.tool_accesses"
	keyDecisions    = "sbo.decisions"
)

// Attributes maps a DecisionRun to its OTLP span attributes. The producer
// (keys.ActorType) is FIXED to "system_agent"; everything else is drawn from r
// with omit-when-empty / omit-when-zero rules and the R5-SF2 length caps.
func Attributes(r run.DecisionRun) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 16)

	// Producer is fixed — NOT derived from r.InitiatedBy.
	attrs = append(attrs, attribute.String(keys.ActorType, capScalar(provenance.ActorSystem.TelemetryValue())))
	attrs = append(attrs, attribute.String(keys.InitiatedBy, capScalar(r.InitiatedBy.TelemetryValue())))
	// RunID and TraceID are the two FREE-FORM correlation scalars: their values
	// come from whatever the emitting decision component had at hand. Both are
	// classified ClassOperationalMetadata by the gateway classify tier, i.e.
	// retained VERBATIM at every capture level (L0 included) — so a path-shaped
	// value here is an unconditional disclosure of a local filesystem path
	// (finding B-B5). run.SanitizeCorrelationID hashes exactly those, leaving
	// ordinary opaque ids (uuids, request ids, "advisor-30") untouched. It is a
	// backstop, not the fix: a producer that KNOWS its value is sensitive hashes
	// at the source with run.CorrelationID.
	attrs = append(attrs, attribute.String(keys.RunID, capScalar(run.SanitizeCorrelationID(r.RunID))))
	attrs = append(attrs, attribute.String(keys.RunTraceID, capScalar(run.SanitizeCorrelationID(r.TraceID))))

	if r.Trigger != "" {
		attrs = append(attrs, attribute.String(keys.Trigger, capScalar(r.Trigger)))
	}
	if r.Component != "" {
		attrs = append(attrs, attribute.String(keys.Component, capScalar(r.Component)))
	}
	if r.Outcome != "" {
		attrs = append(attrs, attribute.String(keys.Outcome, capScalar(r.Outcome)))
	}

	if r.Model != "" {
		attrs = append(attrs, attribute.String(keyGenAIModel, capScalar(r.Model)))
	}
	if r.Provider != "" {
		attrs = append(attrs, attribute.String(keyGenAISystem, capScalar(r.Provider)))
	}
	if r.InputTokens != 0 {
		attrs = append(attrs, attribute.Int64(keyGenAIInputToks, r.InputTokens))
	}
	if r.OutputTokens != 0 {
		attrs = append(attrs, attribute.Int64(keyGenAIOutputToks, r.OutputTokens))
	}
	if r.TotalTokens != 0 {
		attrs = append(attrs, attribute.Int64(keyGenAITotalToks, r.TotalTokens))
	}
	if r.CostUSD != 0 {
		attrs = append(attrs, attribute.Float64(keys.CostUSD, r.CostUSD))
	}
	// Latency is always emitted (a 0 latency is a meaningful observation).
	attrs = append(attrs, attribute.Int64(keys.LatencyMS, r.LatencyMS))

	if len(r.ToolAccesses) > 0 {
		attrs = append(attrs, attribute.StringSlice(keyToolAccesses, capArray(r.ToolAccesses)))
	}
	if len(r.Decisions) > 0 {
		attrs = append(attrs, attribute.StringSlice(keyDecisions, capArray(r.Decisions)))
	}

	return attrs
}

// capScalar truncates s to at most maxScalarValueLen bytes on a rune boundary.
func capScalar(s string) string { return truncateBytes(s, maxScalarValueLen) }

// capArray caps element count to maxArrayElems and each element to
// maxArrayElemLen bytes (rune-boundary). It returns a fresh slice and never
// mutates the input.
func capArray(in []string) []string {
	n := len(in)
	if n > maxArrayElems {
		n = maxArrayElems
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = truncateBytes(in[i], maxArrayElemLen)
	}
	return out
}

// truncateBytes returns the largest prefix of s that is at most max bytes and
// ends on a valid UTF-8 rune boundary (never splitting a multi-byte rune).
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := max
	// Back off until s[b] begins a rune, so s[:b] holds only complete runes.
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b]
}
