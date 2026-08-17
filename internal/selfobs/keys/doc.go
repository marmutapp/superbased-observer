// Package keys is the single source of truth for the self-observability
// operational attribute key registry (club plan SHARED SPEC + R4-B4).
//
// The sbo.* scalar keys are defined ONCE here as Go constants. Both the gateway
// classify allow-list (internal/orgserver/gateway/classify) and the OTLP
// attribute shaper (internal/selfobs/shape) reference these constants — never
// independent string literals — and a cross-package contract test proves they
// cannot diverge.
//
// The package is a pure leaf: it imports NOTHING (not even OpenTelemetry). Its
// imports_test forbids database/sql, net/http, fsnotify, and go.opentelemetry.io.
package keys
