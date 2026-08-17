// Package provenance defines the canonical actor-type provenance taxonomy for
// the Plane-A observability plane (ADR-0004 §2; the P1-2 taxonomy slice).
//
// An actor type answers "what kind of principal acted?" and is an ADDITIONAL
// provenance dimension layered over the machine-identity edge (service_account
// + ingest_credential) documented in
// docs/plane-a/identity-and-resource-model.md §4/§5. It is distinct from the
// per-vendor "actor_type" analytics columns (copilotanalytics /
// m365copilotanalytics / ccanalytics / codexanalytics), which are unrelated
// vendor vocabularies.
//
// The package is a pure DATA leaf (CLAUDE.md module-boundary discipline #1/#5),
// mirroring internal/integration: no database/sql, no net/http, no fsnotify.
// The load-bearing surfacing is TelemetryValue, which maps the canonical
// ActorSystem token ("system") to its telemetry form ("system_agent"); every
// other actor type surfaces as its own canonical token.
package provenance
