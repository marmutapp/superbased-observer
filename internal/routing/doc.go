// Package routing is the model-routing intelligence layer described in
// docs/model-routing-spec.md (§R1–§R26).
//
// The package is pure logic (spec §R5 + superbased-final-spec-v2.md §24.1):
// it receives a Snapshot (the periodically refreshed, immutable view of
// signals) plus a DecisionInput (request shape + session state) and returns
// a Decision. It performs no I/O — no DB, no HTTP, no filesystem; pricing
// is injected as a PriceFn and evidence rows arrive through caller-supplied
// structs. All decision logic is table-driven (rules walked top-down, first
// match wins) per §24.5.
//
// Phase P0 (§R22) is advisory intelligence only: the types and engine here
// are exercised by counterfactual replay (`observer routing simulate`), the
// Model Value Report, and read-only recommendation surfaces. Nothing in P0
// touches the proxy hot path, mutates a request, or routes live traffic —
// Channel B enforcement (§R11) is P1 and reaches this package through one
// seam (proxy.Options.ModelRouter) when it ships.
//
// Capability flags, not identity (§24.3): the engine never sees a tool or
// adapter name. Source differences (provider shape, sidechain, client-
// declared phase, command class) are resolved at the boundary into the
// flags on TurnShape / SessionState / ActionSignal that the rule tables
// consume.
package routing
