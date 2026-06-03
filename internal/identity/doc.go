// Package identity provides the agent's org-enrolment identity and the
// row-stamping seam used to attribute ingested data to an organisation
// and user when the agent is enrolled in an org (Teams & Org Visibility).
//
// The package is deliberately dependency-light: it imports only the
// standard library (context, database/sql). It does NOT import
// internal/models — the row types it stamps satisfy the tiny [OrgRow]
// interface via thin adapters defined in internal/store, which keeps the
// import graph acyclic (store → identity, store → models; identity → ∅).
//
// A [Stamper] is constructed once at daemon start from the org_enrolment
// table. On a solo-local install (no enrolment row, or the table not yet
// created) construction yields a no-op Stamper and a nil error: stamping
// is then a no-op and ingest behaves exactly as it did before org mode
// existed. Nothing in this package panics or fails ingest.
package identity
