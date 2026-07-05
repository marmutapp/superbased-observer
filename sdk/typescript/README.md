# SuperBased Observer — TypeScript SDK

A thin convenience layer over OpenTelemetry (Node) that sends your custom app /
agent traces to a local SuperBased Observer via OTLP, surfacing them on the
**Trajectories** dashboard next to Observer's proxy-accurate cost/cache/routing.

> **Provisional package name** (`@superbased/observer-sdk`) — final SDK naming
> is an open decision (plan §15 Q1).

## Install & build

```bash
cd sdk/typescript
npm install
npm run build      # tsc → dist/
```

## Prerequisites

A local Observer with the subsystem on (`[observability] enabled = true`); the
OTLP receiver listens on `127.0.0.1:4318` (HTTP). The SDK posts to
`http://127.0.0.1:4318/v1/traces`.

## Use

```ts
import { init, Kind, setUsage, shutdown, withLlmSpan, withSpan } from "@superbased/observer-sdk";

init({ sessionId: "run-42", user: "alice" });   // once at startup

await withSpan("plan", Kind.AGENT, async () => {
  await withLlmSpan("chat", { model: "gpt-4o", provider: "openai" }, async (span) => {
    // ... your model call ...
    setUsage(span, { inputTokens: 1200, outputTokens: 80, responseId: "chatcmpl-abc" });
  });
});

await shutdown();   // flush before a short-lived process exits
```

## How it maps

LLM spans are tagged in **both** the OTel GenAI (`gen_ai.*`) and Arize
OpenInference (`llm.*` / `openinference.span.kind`) vocabularies, so Observer's
mapper normalizes them regardless of convention. `responseId` (the provider
message id) doubles as the dedup key against the proxy's `api_turns` when no
`requestId` is set.

## Design test

Anything this SDK does, a stock OTel exporter pointed at Observer does too —
the SDK is pure ergonomics. Direct third-party OTLP exporters are a supported
tier (plan §15 Q2).

## Echo-guard caveat

Never set the resource attribute `sbo.emitted_by=observer` — Observer's own
marker; such a resource is **dropped** at ingestion. The SDK sets
`sbo.sdk="superbased-typescript"` instead.
