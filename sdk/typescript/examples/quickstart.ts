// Minimal SuperBased Observer TypeScript SDK example.
//
//   cd sdk/typescript && npm install && npm run build
//   node --loader ts-node/esm examples/quickstart.ts   # or compile + run
//
// Requires a local Observer with [observability] enabled.

import { init, Kind, setUsage, shutdown, withLlmSpan, withSpan } from "../src/index.js";

async function main() {
  init({ sessionId: "quickstart-1", user: "demo" });

  await withSpan("agent.run", Kind.AGENT, async () => {
    await withLlmSpan("chat", { model: "gpt-4o", provider: "openai" }, async (span) => {
      // ... your real model call ...
      setUsage(span, { inputTokens: 1200, outputTokens: 64, responseId: "chatcmpl-demo-123" });
    });
    await withSpan("web_search", Kind.TOOL, async () => {
      // ... your tool call ...
    });
  });

  await shutdown();
  console.log("sent one trace to Observer — check /trajectories");
}

void main();
