// Package egress is the PURE egress.routing_guardrail family compiler
// (docs/plane-a/unified-policy-resource.md §3): it turns a plain, versioned
// wire body into the ready-to-evaluate PolicySpec that internal/obs/egress
// already knows how to run.
//
// It is the extracted core of what used to live directly in
// internal/obs/egress's policy.go + types.go + lint.go: PolicyInput/
// PolicySpec/CompiledRule/CompiledWhen, the Action/ProviderShape/Effort/
// ReasonCode enums + their validators, Target, Compile, and Lint.
// internal/obs/egress now imports this package and re-exports every one of
// those names via a type alias or a package-level var (see its policy.go),
// so no existing call site — inside or outside internal/obs — changed
// shape (Plane-A P0-5 Phase F,
// docs/plans/plane-a-p0-5-unified-policy-resource-v1-plan.md).
//
// Purity (CLAUDE.md #1, design §3.1): beyond the admission family's forbid
// list (database/sql, net/http, fsnotify, internal/obs), this package ALSO
// forbids internal/routing and internal/obs/admission — pinned by
// imports_test.go. The routing vocabulary (effort levels, provider shapes,
// first-match discipline, closed reason codes) is RE-EXPRESSED here as
// small Plane-A-native string enums so no routing type crosses any seam,
// and no admission type crosses the family-compiler boundary either — the
// admission verdict enters Evaluate (which stays in internal/obs/egress) as
// plain strings.
//
// BodyV1 + DecodeBody + CompileBody are the new v1 wire boundary
// (unified-policy-resource.md §6). The wire body NESTS a rule's action
// fields under an "action" object (mirroring internal/obs/httpapi's
// dashboard editor DTO) rather than flattening them like RuleInput;
// ToPolicyInput maps that nested shape onto the flat RuleInput the engine
// compiles, at this one boundary.
package egress
