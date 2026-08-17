# SuperBased Observer - TypeScript SDK

A thin convenience layer over OpenTelemetry (Node). This SDK sends your
custom app / agent traces to a local SuperBased Observer via OTLP.

> **Provisional package name** (`@superbased/observer-sdk`), final SDK naming
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
  await withLlmSpan("chat", { model: "gpt-4o", provider: "openai", prompt: userMessage }, async (span) => {
    // ... your model call ...
    setUsage(span, { inputTokens: 1200, outputTokens: 80, responseId: "chatcmpl-abc", response: assistantReply });
  });
});

await shutdown();   // flush before a short-lived process exits
```

## Content capture

Prompt/response bodies are captured **by default** when you pass them to the
span helpers (`withLlmSpan(name, { prompt }, ...)`, `setUsage(span, { prompt, response })`,
or `setContent(span, { prompt, response })`, the last also takes `toolArgs` /
`toolResult` for TOOL spans). Observer persists them subject to your node's
content-retention posture.

Turn capture **off** two ways (either wins; explicit option beats env):

```ts
init({ captureContent: false });        // per-process option
// or:  export OBSERVER_CAPTURE_CONTENT=0   (also: false / no / off)
```

Each body is clipped client-side to `maxContentChars` (default 8000;
`init({ maxContentChars: 0 })` disables the clip) so a giant prompt never
bloats the OTLP export.

## Pre-flight check

`admit()` posts a message to an optional pre-flight check endpoint on your
Observer deployment and returns an allow/flag/ask/deny `Verdict`. Call it at
your app's front door before running the agent:

```ts
const v = await admit(userMessage, { user: "alice", session: "s1" });
if (!v.allowed) return v.reason;
```

Defaults to `http://127.0.0.1:8081/api/obs/admission/check` (the local node
dashboard's port; override with `opts.endpoint` or
`$SUPERBASED_ADMISSION_ENDPOINT`). Fails **open** on any transport error: an
unreachable Observer never blocks your app.

## How it maps

LLM spans are tagged in **both** the OTel GenAI (`gen_ai.*`) and Arize
OpenInference (`llm.*` / `openinference.span.kind`) vocabularies, so Observer's
mapper normalizes them regardless of convention. `responseId` (the provider
message id) doubles as the dedup key against the proxy's `api_turns` when no
`requestId` is set.

## Design test

Anything this SDK does, a stock OTel exporter pointed at Observer does too;
the SDK is pure ergonomics. Direct third-party OTLP exporters are a supported
tier (plan §15 Q2).

## Echo-guard caveat

Never set the resource attribute `sbo.emitted_by=observer`; it is Observer's
own marker for telemetry it emitted, and any resource carrying it is
**dropped** at ingestion. The SDK sets `sbo.sdk="superbased-typescript"`
instead.
