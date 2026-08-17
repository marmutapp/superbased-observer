// Package shape is the ONE pure shaper from a self-observability
// run.DecisionRun to OTLP span attributes.
//
// It is the only place the DecisionRun→attribute mapping lives; run and keys
// stay OTel-free. The producer attribute (keys.ActorType) is FIXED to
// provenance.ActorSystem.TelemetryValue() ("system_agent") — never derived from
// r.InitiatedBy. Scalar values are byte-length-capped and arrays are
// element-count- and per-element-byte-capped (see the bound consts), always
// truncating on a valid UTF-8 rune boundary.
//
// The package MAY import go.opentelemetry.io/otel/attribute; its imports_test
// forbids database/sql, net/http, fsnotify (OTel allowed) and it must not import
// internal/obs.
package shape
