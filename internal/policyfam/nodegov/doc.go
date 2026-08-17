// Package nodegov is the PURE node.governance family compiler
// (docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §3, Phase 1a): it
// turns a plain, versioned wire body describing which node-dashboard
// sections an enrolled node's organization hides or locks into the
// ready-to-apply PolicySpec the cmd/observer install seam hands to
// internal/govern.Resolve.
//
// Phase 1a carries SECTION VISIBILITY + NOTICE ONLY. The spec's `pinned`,
// `share` and `features` directive classes are deliberately absent from
// schema 1: they would each need a delivery class that is not fully hot
// (a config overlay read by ~20 short-lived config.Load call sites, a
// restart-bound orgclient share posture, restart-bound subsystem wiring),
// and the adversarial review showed the config overlay is defeated in the
// hook and MCP processes that do the actual enforcing. Those classes land
// in Phase 1b behind a schema bump.
//
// Purity (CLAUDE.md #1): this package imports no database/sql, net/http,
// fsnotify, internal/config, internal/obs, internal/proxy or
// internal/store — pinned by imports_test.go. It imports net/url only to
// VALIDATE that the notice's policy_url is an absolute http/https URL; it
// never dials anything.
//
// BodyV1 + DecodeBody + CompileBody are the v1 wire boundary, mirroring
// policyfam/providers: the raw JSON an org publisher POSTs (and an agent
// later fetches inside a SignedPolicyResource.Body) is strictly decoded,
// canonicalized, and compiled — so BodyHash is always computed over the
// canonical bytes, never over the publisher's raw submission.
//
// Two closed vocabularies live here (vocab.go): the node dashboard's nav
// section ids and its Settings sub-section ids. They are asserted against
// web/src/lib/nav.ts and web/src/pages/Settings.tsx by vocab_source_test.go
// rather than hand-maintained in isolation, because an unknown id is a HARD
// compile error on both the publish and the agent side (an admin must never
// believe a page is hidden when it is not), which would make a silent
// vocabulary drift a fleet-wide outage.
package nodegov
