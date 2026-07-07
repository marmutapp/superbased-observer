// Package ccotel parses Claude Code's native OpenTelemetry export into Observer
// API-turn observations (native-console integration, Workstream A / Phase 2b).
//
// When a customer sets CLAUDE_CODE_ENABLE_TELEMETRY=1 and points Claude Code's
// OTLP exporter at Observer's receiver, Claude Code emits each turn as a log
// record — the api_request event — carrying the Anthropic request-id header plus
// exact token counts and cost. This package walks an OTLP logs export and maps
// those events to models.APITurn with Source set to the cc_otel provenance tag,
// so the store's UpsertTurnByRequestID can dedup them against proxy/JSONL rows
// by request_id (internal/turnmerge precedence).
//
// It is pure: it decodes already-unmarshaled OTLP proto messages and returns
// plain models, with no network, DB, or fsnotify. The OTLP listener that feeds
// it lives in internal/ingest/otlp. Attribute lookups try several candidate
// keys because Claude Code's attribute names have drifted across versions.
package ccotel
