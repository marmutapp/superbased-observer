package keys

// The self-observability operational scalar attribute keys, retained verbatim
// (bounded) as ClassOperationalMetadata by the gateway classify tier. See the
// club plan SHARED SPEC and R4-B4.
const (
	// ActorType is always "system_agent" — the run's producer is fixed.
	ActorType = "sbo.actor.type"
	// InitiatedBy is the initiating actor's TelemetryValue().
	InitiatedBy = "sbo.initiated_by"
	// RunID is the decision-run id.
	RunID = "sbo.run.id"
	// RunTraceID is a persisted CORRELATION id (NOT the OTel trace id).
	RunTraceID = "sbo.run.trace_id"
	// Trigger is cron|webhook|manual|anomaly.
	Trigger = "sbo.trigger"
	// Component is the emitting decision component name.
	Component = "sbo.component"
	// Outcome is accepted|rejected|dismissed|verified|"".
	Outcome = "sbo.outcome"
	// CostUSD is the run's cost. Retained operational via the generic "cost"
	// substring classify rule (NeedsExactRule:false).
	CostUSD = "sbo.cost.usd"
	// LatencyMS is the run's latency. Retained operational via the generic
	// "latency" substring classify rule (NeedsExactRule:false).
	LatencyMS = "sbo.latency_ms"
)

// RetainedKey is one operational scalar the shaper emits and the classify tier
// must retain (ClassOperationalMetadata). NeedsExactRule marks keys requiring a
// dedicated exact-match classify rule (the 7 sbo scalars, which would otherwise
// fall through to ClassCustomerContent). CostUSD/LatencyMS are already
// ClassOperationalMetadata via the generic "cost"/"latency" substring rules, so
// NeedsExactRule:false.
type RetainedKey struct {
	Key            string
	NeedsExactRule bool
}

// Retained is the ONE registry of retained operational scalar keys — the single
// source of truth consumed by the classify allow-list, the shaper, and the
// cross-package contract test (R4-B4). No second vocabulary.
var Retained = []RetainedKey{
	{ActorType, true},
	{InitiatedBy, true},
	{RunID, true},
	{RunTraceID, true},
	{Trigger, true},
	{Component, true},
	{Outcome, true},
	{CostUSD, false},
	{LatencyMS, false},
}

// ExactRuleKeys returns the keys needing an explicit exact-match classify rule
// (the keys with NeedsExactRule:true), in the stable declaration order of
// Retained. The classify allow-list is generated from this — single source of
// truth.
func ExactRuleKeys() []string {
	out := make([]string, 0, len(Retained))
	for _, k := range Retained {
		if k.NeedsExactRule {
			out = append(out, k.Key)
		}
	}
	return out
}
