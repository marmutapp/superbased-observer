// Package evalcore is the pure scorer core shared by the node eval plane
// (internal/obs/eval, which re-exports this package's public API for its
// existing callers) and the org-authored eval service
// (internal/orgserver/orgeval). It holds the Sample/Score/Result/Spec types,
// the scorer registry (exact_match, contains, icontains, regex_match,
// json_valid, non_empty, status_ok, latency_under, cost_under, llm_judge),
// and the Run/Summarize orchestration.
//
// This package imports no database/sql, net/http, or fsnotify — persistence
// and the judge's outbound network call are the caller's responsibility,
// reached only via the injected JudgeClient interface. See
// docs/plans/org-eval-service-comprehensive-plan-2026-08-20.md §2.1 for the
// extraction rationale (a low-risk, mechanical move: internal/obs's reverse-
// import boundary forbids internal/orgserver from importing internal/obs/eval
// directly, so the pure, zero-domain-coupling parts of that package moved
// here instead of being duplicated).
package evalcore
