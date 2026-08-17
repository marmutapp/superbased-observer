package provenance

import "fmt"

// ActorType is the canonical provenance taxonomy for a principal that acts in
// the Plane-A plane. The zero value is not a valid actor type.
type ActorType string

// The five canonical actor types (ADR-0004 §2).
const (
	// ActorHuman is a human operator acting directly.
	ActorHuman ActorType = "human"
	// ActorCodingAgent is a developer-facing coding agent (Plane-B tools).
	ActorCodingAgent ActorType = "coding_agent"
	// ActorPolicyAgent is an automated policy/enforcement agent.
	ActorPolicyAgent ActorType = "policy_agent"
	// ActorInsightAgent is an automated analysis/insight agent.
	ActorInsightAgent ActorType = "insight_agent"
	// ActorSystem is the platform itself acting as a system agent. Its
	// telemetry surfacing is "system_agent" (see TelemetryValue).
	ActorSystem ActorType = "system"
)

// AllActorTypes enumerates every canonical actor type, in declaration order.
var AllActorTypes = []ActorType{
	ActorHuman,
	ActorCodingAgent,
	ActorPolicyAgent,
	ActorInsightAgent,
	ActorSystem,
}

// String returns the canonical token for the actor type (== string(a)). It does
// NOT apply the system→system_agent telemetry surfacing; use TelemetryValue for
// the emitted attribute value.
func (a ActorType) String() string { return string(a) }

// Valid reports whether a is one of the canonical actor types.
func (a ActorType) Valid() bool {
	switch a {
	case ActorHuman, ActorCodingAgent, ActorPolicyAgent, ActorInsightAgent, ActorSystem:
		return true
	default:
		return false
	}
}

// Parse resolves a canonical token into its ActorType, erroring on any unknown
// value.
func Parse(s string) (ActorType, error) {
	a := ActorType(s)
	if !a.Valid() {
		return "", fmt.Errorf("provenance.Parse: unknown actor type %q", s)
	}
	return a, nil
}

// TelemetryValue returns the value this actor type surfaces as in telemetry.
// ActorSystem surfaces as "system_agent" (the load-bearing mapping); every other
// actor type surfaces as its own canonical token. This is the ONLY place the
// system→system_agent surfacing is applied.
func (a ActorType) TelemetryValue() string {
	if a == ActorSystem {
		return "system_agent"
	}
	return string(a)
}
