// Package admission is the PURE admission.input / admission.output family
// compiler (docs/plane-a/unified-policy-resource.md §3): it turns a plain,
// versioned wire body into the ready-to-evaluate PolicySpec that
// internal/obs/admission already knows how to run.
//
// It is the extracted core of what used to live directly in
// internal/obs/admission's policy.go + lint.go: PolicyInput/PolicySpec/
// Criterion/Prefilter, the Decision/Severity/Mode/Scope/CriterionType
// enums, Compile, and Lint. internal/obs/admission now imports this
// package and re-exports every one of those names via a type alias or a
// package-level var (see its policy.go), so no existing call site — inside
// or outside internal/obs — changed shape (Plane-A P0-5 Phase F,
// docs/plans/plane-a-p0-5-unified-policy-resource-v1-plan.md).
//
// Purity (CLAUDE.md #1, obs plan §11): this package imports no
// database/sql, net/http, fsnotify, or internal/obs — pinned by
// imports_test.go.
//
// BodyV1 + DecodeBody + CompileBody are the new v1 wire boundary
// (unified-policy-resource.md §6): they turn the raw JSON an org publisher
// POSTs (and an agent later fetches inside a SignedPolicyResource.Body)
// into a strictly-decoded, canonicalized BodyV1 and then a compiled
// PolicySpec — so internal/orgserver and internal/orgclient can compile a
// resource body without ever importing internal/obs. RequiresJudge /
// ValidateRuntimeCaps let a caller reject a body whose judged criteria the
// runtime cannot honor (capability_mismatch, plan §6.6) before it is ever
// installed.
package admission
