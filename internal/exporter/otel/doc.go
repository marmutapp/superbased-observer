// Package otel is the agent-side OpenTelemetry exporter — the "second rail"
// of the Teams & Org Visibility feature (spec §2.4.3). It tails the local
// store's api_turns table and emits one gen_ai.client span per turn to any
// OTLP/HTTP endpoint (Datadog, Grafana/Tempo, Honeycomb, Dynatrace, SigNoz,
// or a plain OpenTelemetry Collector), so customers with an existing
// observability backend can ingest SuperBased Observer's data without running
// the org server.
//
// # Independence
//
// The exporter requires only M0 (the local store). It works identically on a
// solo-local install and on an enrolled agent, and it never imports an
// orgserver package. When [exporter.otel] enabled is false (the default), the
// daemon starts no exporter goroutine and the process makes zero OTLP network
// calls — the solo-local UX stays byte-identical.
//
// # Span shape
//
// One span named "chat <model>" per api_turns row. Standard GenAI semantic
// convention attributes (gen_ai.*) plus a small SuperBased custom namespace
// (sbo.*). The full mapping lives in attributes.go; the headline set is:
//
//   - gen_ai.provider.name, gen_ai.request.model, gen_ai.response.model,
//     gen_ai.operation.name, gen_ai.usage.input_tokens,
//     gen_ai.usage.output_tokens, gen_ai.response.id,
//     gen_ai.response.finish_reasons.
//   - sbo.project.root, sbo.session.id, sbo.cost.usd, sbo.cache.read_tokens,
//     sbo.web_search.requests, sbo.tool.adapter, sbo.action.normalized_type,
//     sbo.freshness.classification, sbo.redundancy.is_stale_reread, and — only
//     when the agent is enrolled — sbo.org.id, plus sbo.user.email when
//     EmitUserEmail is set.
//
// The span carries the turn-intrinsic attributes; the action-level sbo.*
// attributes (freshness, redundancy, normalized_type, adapter) are produced by
// the same [Attributes] mapping function when an Action is supplied, so the
// attribute vocabulary has a single home and is unit-tested as a unit. The
// turn tail itself supplies a nil Action.
//
// # GenAI semantic conventions
//
// The exporter emits the v1.41.0 GenAI attribute names. As of this writing the
// OpenTelemetry contrib repo does not publish a Go package for the GenAI
// semantic conventions (go.opentelemetry.io/contrib/instrumentation/genai/
// semconv does not exist at any tagged version), so the gen_ai.* attribute
// keys are defined as constants in attributes.go against the v1.41.0 spec.
// General resource attributes use the released semconv package. Per the OTel
// transition plan the exporter advertises OTEL_SEMCONV_STABILITY_OPT_IN=
// gen_ai_latest_experimental by default; the operator may override it via the
// environment variable.
//
// # Prompt-content emission (opt-in, default OFF)
//
// When EmitPromptContent is true the exporter attaches a
// gen_ai.client.inference.operation.details event to each span, per the OTel
// events-not-spans rule for message content. This is OFF by default.
//
// Note on what is actually emitted: SuperBased Observer does not persist raw
// prompt or completion bodies (the no-content-in-DB invariant — only paths,
// commands, hashes, and capped excerpts are stored), so this event carries the
// available content metadata (the stable system-prompt and message-prefix
// hashes, and any error message) rather than the raw bodies the OTel convention
// envisions. A deployment that needs full message bodies in its backend must
// capture them at the proxy, before they are discarded; were that wired, the
// data-volume warning would apply in full — message bodies are large and
// unbounded, multiplying span payload size and backend storage by one to three
// orders of magnitude versus the attribute-only span, and they contain the
// developer's source code, prompts, and tool output.
//
// # Failure isolation (P1)
//
// The exporter is a strictly best-effort consumer. Construction or export
// failures are logged at WARN and never propagate to the daemon; a stuck
// collector, a DNS failure, or a malformed endpoint must never affect ingest,
// the proxy, the watcher, the dashboard, or the org push loop. The row tail
// resumes from a persisted cursor (otel_cursor in schema_meta) across restarts,
// so a backend outage does not lose turns — they export when the backend
// returns.
package otel
