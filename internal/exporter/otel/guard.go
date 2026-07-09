package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Guard-event emission ([guard.export].otel, guard spec §11.4 — G16).
// When BOTH [exporter.otel] is enabled AND [guard.export].otel is
// true, the daemon tails guard_events (cmd/observer/guardotel.go, the
// cloud-dispatcher sweep pattern) and emits one short span per audit
// row through this exporter's existing OTLP rail. The double gate
// preserves the solo-local no-network invariant twice over: each
// flag's default is off.
//
// Boundary note (§15 one-owner rule): this is NOT a [guard.cloud]
// feature and deliberately does not route through notify.Egress —
// guard events here join the EXISTING Teams-M4 exporter rail the
// operator separately opted into, exactly as §11.4 places it ("adds
// guard events to the OTel surface"); §15.4's push alerting remains
// the notify.Egress webhook surface that shipped with G15.

// guardSpanPrefix names guard spans "guard <rule_id>".
const guardSpanPrefix = "guard"

// SuperBased guard-namespace attribute keys (sbo.guard.*). Mirrors
// the guard_events audit-row vocabulary; content-bearing fields
// (reason, target excerpt, taint origin) are gated separately — see
// ExportGuardEvent.
const (
	keySBOGuardEventID       = "sbo.guard.event_id"
	keySBOGuardRuleID        = "sbo.guard.rule_id"
	keySBOGuardCategory      = "sbo.guard.category"
	keySBOGuardSeverity      = "sbo.guard.severity"
	keySBOGuardDecision      = "sbo.guard.decision"
	keySBOGuardDegradedFrom  = "sbo.guard.degraded_from"
	keySBOGuardEnforced      = "sbo.guard.enforced"
	keySBOGuardSource        = "sbo.guard.source"
	keySBOGuardEventKind     = "sbo.guard.event_kind"
	keySBOGuardTargetHash    = "sbo.guard.target_hash"
	keySBOGuardChainHash     = "sbo.guard.chain_hash"
	keySBOGuardReason        = "sbo.guard.reason"
	keySBOGuardTargetExcerpt = "sbo.guard.target_excerpt"
	keySBOGuardTaintOrigin   = "sbo.guard.taint_origin"
)

// GuardEvent is one guard audit event in exporter-facing form. The
// cmd layer maps store guard_events rows onto this type at the
// boundary (the package never imports the store — the TurnSource
// decoupling, applied to the guard tail).
type GuardEvent struct {
	// ID is the guard_events row id.
	ID int64
	// TS is the event time; spans are zero-duration at this instant.
	TS time.Time
	// SessionID / Tool locate the event in the capture model.
	SessionID string
	Tool      string
	// EventKind / RuleID / Category / Severity / Decision /
	// DegradedFrom / Enforced / Source mirror the audit row's verdict
	// fields.
	EventKind    string
	RuleID       string
	Category     string
	Severity     string
	Decision     string
	DegradedFrom string
	Enforced     bool
	Source       string
	// TargetHash / ChainHash are the content-free correlation keys.
	TargetHash string
	ChainHash  string
	// Reason / TargetExcerpt / TaintOrigin are content-bearing and
	// emitted only under the exporter's content gate.
	Reason        string
	TargetExcerpt string
	TaintOrigin   string
}

// GuardSpanName returns the span name for a guard event ("guard
// <rule_id>" — rule ids are stable and low-cardinality, the same
// grouping logic as the GenAI "{operation} {model}" convention).
func GuardSpanName(ev GuardEvent) string {
	if ev.RuleID == "" {
		return guardSpanPrefix
	}
	return guardSpanPrefix + " " + ev.RuleID
}

// guardAttributes maps a guard event onto its span attribute set.
// Content-bearing fields (reason, target excerpt, taint origin) are
// included only when emitContent is true — the exporter's existing
// EmitPromptContent flag is the single content-egress gate, so an
// operator's "no content leaves this machine over OTLP" decision
// covers guard rows too. Hash and enum fields always emit; empty
// optional values are omitted rather than emitted blank.
func guardAttributes(ev GuardEvent, emitContent bool) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 16)
	attrs = append(
		attrs,
		attribute.Int64(keySBOGuardEventID, ev.ID),
		attribute.String(keySBOGuardRuleID, ev.RuleID),
		attribute.Bool(keySBOGuardEnforced, ev.Enforced),
	)
	// Ordered pairs (not a map) so attribute order is deterministic
	// run to run.
	pairs := []struct{ key, val string }{
		{keySBOGuardCategory, ev.Category},
		{keySBOGuardSeverity, ev.Severity},
		{keySBOGuardDecision, ev.Decision},
		{keySBOGuardDegradedFrom, ev.DegradedFrom},
		{keySBOGuardSource, ev.Source},
		{keySBOGuardEventKind, ev.EventKind},
		{keySBOGuardTargetHash, ev.TargetHash},
		{keySBOGuardChainHash, ev.ChainHash},
		{keySBOSessionID, ev.SessionID},
		{keySBOToolAdapter, ev.Tool},
	}
	if emitContent {
		pairs = append(
			pairs,
			struct{ key, val string }{keySBOGuardReason, ev.Reason},
			struct{ key, val string }{keySBOGuardTargetExcerpt, ev.TargetExcerpt},
			struct{ key, val string }{keySBOGuardTaintOrigin, ev.TaintOrigin},
		)
	}
	for _, p := range pairs {
		if p.val != "" {
			attrs = append(attrs, attribute.String(p.key, p.val))
		}
	}
	return attrs
}

// ExportGuardEvent emits one guard audit event as a zero-duration
// span at the event's timestamp. Deny/mask decisions set span status
// Error (the agent's operation was blocked — the failure framing SIEM
// dashboards slice on) with a content-free description; everything
// else stays Unset. Best-effort like the turn tail: the batching SDK
// owns retries, and a lost span never blocks anything (the audit
// chain remains the record).
func (e *Exporter) ExportGuardEvent(ctx context.Context, ev GuardEvent) {
	ts := ev.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	_, span := e.tracer.Start(
		ctx, GuardSpanName(ev),
		trace.WithTimestamp(ts),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(guardAttributes(ev, e.cfg.EmitPromptContent)...),
	)
	if ev.Decision == "deny" || ev.Decision == "mask" {
		span.SetStatus(codes.Error, "blocked by guard rule "+ev.RuleID)
	}
	span.End(trace.WithTimestamp(ts))
}
