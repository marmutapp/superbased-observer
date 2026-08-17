// Package providers is the PURE gateway.providers family compiler
// (docs/plans/gateway-config-plane-spec-2026-08-15.md Phase 3): it turns a
// plain, versioned wire body describing a dashboard-managed proxy lane
// table into the ready-to-apply PolicySpec that internal/proxy.Proxy's
// SetLaneTable installs hot, with no restart.
//
// Unlike policyfam/admission and policyfam/egress, this family has no
// evaluation engine of its own to extract from internal/obs — it exists
// purely so internal/orgserver and internal/orgclient can compile and
// canonicalize a lane-table body without ever importing internal/proxy
// (the reverse-import boundary this package exists to enable).
//
// Purity (CLAUDE.md #1): this package imports no database/sql, net/http,
// fsnotify, internal/config, internal/obs, or internal/proxy — pinned by
// imports_test.go. It imports net/url only to VALIDATE that each base_url
// is an absolute http/https URL; it never dials anything.
//
// BodyV1 + DecodeBody + CompileBody are the v1 wire boundary, mirroring the
// discipline in policyfam/admission and policyfam/egress: the raw JSON an
// org publisher POSTs (and an agent later fetches inside a
// SignedPolicyResource.Body) is strictly decoded, canonicalized, and
// compiled into a PolicySpec whose UpstreamsAsStringMap() is exactly the
// shape internal/proxy's SetLaneTable expects.
package providers
