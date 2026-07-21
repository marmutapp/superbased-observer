// Package egress is the Plane-A policy egress-routing decision layer (G22,
// docs/plans/g22-plane-a-egress-routing-design-2026-07-11.md). Given an
// admission verdict, the requested model/provider, the end-user identity and
// budget burn, and a coarse prompt-size band, it maps that request onto an
// egress directive — route to an alternate declared upstream, swap to a cheaper
// same-shape model, degrade effort, deny-with-message, or exempt.
//
// It is PURE (design §3.1): a table-driven, first-match-wins evaluator with no
// I/O. Purity is pinned by imports_test.go, which — beyond admission's forbid
// list (database/sql, net/http, fsnotify, internal/config, internal/obs/eval) —
// also forbids internal/routing and internal/obs/admission. The routing
// vocabulary (effort levels, provider shapes, first-match discipline, closed
// reason codes) is RE-EXPRESSED here as small Plane-A-native string enums so no
// routing type crosses any seam.
//
// The boundary service (internal/obs/admissionsvc) folds Evaluate into
// AdmissionService.Check and translates the directive into a plain proxy route
// contract; the pure types never leak past that boundary.
package egress
