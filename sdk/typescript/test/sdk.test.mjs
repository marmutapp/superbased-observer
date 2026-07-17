// SDK unit tests — run against the compiled dist (npm test builds first).
// Node's built-in runner: no extra dev dependency.
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_ADMISSION_ENDPOINT,
  init,
  setContent,
  setUsage,
} from "../dist/index.js";

/** Minimal duck-typed Span that records setAttribute calls. */
function fakeSpan() {
  const attrs = {};
  return {
    attrs,
    setAttribute(k, v) {
      attrs[k] = v;
    },
  };
}

test("admission endpoint defaults to the node obs API on :8081", () => {
  assert.equal(
    DEFAULT_ADMISSION_ENDPOINT,
    "http://127.0.0.1:8081/api/obs/admission/check",
  );
});

test("content is attached by default", () => {
  init({ captureContent: true });
  const sp = fakeSpan();
  setContent(sp, { prompt: "hello", response: "world" });
  assert.equal(sp.attrs["input.value"], "hello");
  assert.equal(sp.attrs["output.value"], "world");
});

test("setUsage attaches prompt/response content by default", () => {
  init({ captureContent: true });
  const sp = fakeSpan();
  setUsage(sp, { inputTokens: 10, outputTokens: 2, prompt: "p", response: "r" });
  assert.equal(sp.attrs["gen_ai.usage.input_tokens"], 10);
  assert.equal(sp.attrs["input.value"], "p");
  assert.equal(sp.attrs["output.value"], "r");
});

test("off-switch (captureContent:false) suppresses content, keeps usage", () => {
  init({ captureContent: false });
  const sp = fakeSpan();
  setUsage(sp, { inputTokens: 5, prompt: "secret", response: "secret" });
  assert.equal(sp.attrs["gen_ai.usage.input_tokens"], 5);
  assert.equal(sp.attrs["input.value"], undefined);
  assert.equal(sp.attrs["output.value"], undefined);
});

test("tool args/result map to input.value/output.value", () => {
  init({ captureContent: true });
  const sp = fakeSpan();
  setContent(sp, { toolArgs: "{\"q\":1}", toolResult: "ok" });
  assert.equal(sp.attrs["input.value"], "{\"q\":1}");
  assert.equal(sp.attrs["output.value"], "ok");
});

test("bodies are clipped to maxContentChars", () => {
  init({ captureContent: true, maxContentChars: 5 });
  const sp = fakeSpan();
  setContent(sp, { prompt: "abcdefghij" });
  assert.equal(sp.attrs["input.value"], "abcde…[truncated]");
  // restore default cap for any later tests
  init({ captureContent: true });
});
