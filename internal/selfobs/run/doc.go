// Package run holds the pure DecisionRun value that describes a single
// platform self-observability decision run (routing/advisor/admission/eval/
// insight-agent, and the synthetic reference conformer).
//
// A DecisionRun is ITSELF always a system_agent producer (provenance.ActorSystem);
// InitiatedBy is a SEPARATE field naming who initiated the run. Validate stamps
// and asserts that fixed producer.
//
// The package is pure and OTel-FREE: it imports internal/provenance only. The
// DecisionRun→OTLP attribute shaping lives in internal/selfobs/shape, not here.
// imports_test forbids OpenTelemetry, database/sql, net/http, and fsnotify.
package run
