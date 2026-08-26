// Package nodefeatures is the PURE node.features family compiler + decision
// helper (org-parity plan W5.1,
// docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W5.1"): the org
// admin DECIDES, per governed local capability (embedded terminals, remote
// pairing, routing-apply, patterns-write), whether a dev's node may use it
// and with what limits — authored as policy and distributed on the existing
// policy-resource rail; the node ENFORCES at its own local seams and ACKs
// via policy_state. There is never a remote-drive path here: this family
// can only say "no" (or "yes, with a limit"), never reach out and DO
// anything on the node's behalf.
//
// Unlike policyfam/admission and policyfam/egress, this family's
// "evaluation engine" is trivial and has no internal/obs counterpart to
// extract — evaluating a compiled spec against one feature name is a
// handful of field reads, not a boundary-owned engine, so (mirroring how
// policyfam/providers bundles its own small HashLaneTable helper rather
// than needing a separate package) the pure decision function Allowed lives
// in this same package alongside the wire compiler, rather than being
// split out.
//
// Purity (CLAUDE.md #1): this package imports no database/sql, net/http,
// fsnotify, internal/config, internal/obs, or internal/proxy — pinned by
// imports_test.go.
//
// BodyV1 + DecodeBody + CompileBody are the v1 wire boundary, mirroring the
// discipline in policyfam/admission, policyfam/egress, and
// policyfam/providers: the raw JSON an org publisher POSTs (and an agent
// later fetches inside a SignedPolicyResource.Body) is strictly decoded,
// canonicalized, and compiled into a PolicySpec.
package nodefeatures
