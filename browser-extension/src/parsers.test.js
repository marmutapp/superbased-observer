// SPDX-License-Identifier: Apache-2.0
// Copyright (c) SuperBased. Part of SuperBased Observer.
//
// Unit tests for the per-site stream parsers (src/parsers.js). The fixtures
// are SYNTHETIC frames modeled on the July-2026 reverse-engineering research
// (docs/plans/browser-extension-stream-shapes-2026-07-10.md), NOT real user
// data. They pin the [high]-confidence structural shapes so a live capture
// only has to confirm the flagged TODO(must-verify-live) items — the framing
// itself is regression-guarded here. Run: `node src/parsers.test.js`.
"use strict";

const assert = require("assert");
const P = require("./parsers.js");

let passed = 0;
function ok(name, cond) {
  assert.ok(cond, "FAIL: " + name);
  passed++;
}
function eq(name, got, want) {
  assert.strictEqual(got, want, "FAIL: " + name + " — got " + JSON.stringify(got) + " want " + JSON.stringify(want));
  passed++;
}

// feed splits an SSE string into a couple of chunks to exercise the streaming
// path (frame boundaries do not align to chunk boundaries in real life).
function runSSE(acc, sse) {
  const mid = Math.floor(sse.length / 2);
  acc.feed(sse.slice(0, mid));
  acc.feed(sse.slice(mid));
  if (acc.finalize) acc.finalize();
  return acc.state;
}

function dataLines(objs) {
  // objs may be strings (already-serialized, for text: fields) or objects.
  return (
    objs
      .map((o) => "data: " + (typeof o === "string" ? o : JSON.stringify(o)))
      .join("\n") + "\n"
  );
}

// ============================ ChatGPT ==================================
(function chatgpt() {
  // The full happy path: user echo (excluded) → assistant snapshot → an
  // explicit append → STICKY deltas (o/p omitted) → a patch batch.
  const stream = dataLines([
    // (1) input_message echo of the USER turn — must be EXCLUDED.
    {
      v: {
        message: {
          id: "msg_user",
          author: { role: "user" },
          content: { content_type: "text", parts: ["IGNORE THIS USER ECHO"] },
        },
        conversation_id: "conv_1",
      },
    },
    // (2) assistant snapshot — id + model_slug, empty parts at stream start.
    {
      p: "",
      o: "add",
      v: {
        message: {
          id: "msg_asst",
          author: { role: "assistant" },
          content: { content_type: "text", parts: [""] },
          metadata: { model_slug: "gpt-5-6-thinking" },
        },
        conversation_id: "conv_1",
      },
    },
    // (3) first explicit text delta.
    { p: "/message/content/parts/0", o: "append", v: "Hello" },
    // (4) STICKY deltas — o AND p omitted, inherit from the previous frame.
    { v: ", world" },
    { v: "!" },
    // (5) patch batch — v is an ARRAY of sub-ops.
    {
      o: "patch",
      p: "",
      v: [
        { p: "/message/content/parts/0", o: "append", v: " Bye" },
        { p: "/message/status", o: "replace", v: "finished_successfully" },
      ],
    },
    "[DONE]",
  ]);
  const s = runSSE(P.makeChatGPTAccumulator(), stream);
  eq("chatgpt sticky-frame text assembled", s.text, "Hello, world! Bye");
  ok("chatgpt user echo excluded", s.text.indexOf("IGNORE") === -1);
  eq("chatgpt assistant message id", s.messageId, "msg_asst");
  eq("chatgpt model_slug", s.model, "gpt-5-6-thinking");
  eq("chatgpt conversation id", s.conversationId, "conv_1");

  // Sticky-only stress: one explicit append followed by MANY sticky frames —
  // the #1 silent-failure mode a require-o+p parser drops.
  const many = ["First"];
  const lines = [
    { p: "/message/content/parts/0", o: "append", v: "First" },
  ];
  for (let i = 0; i < 20; i++) {
    lines.push({ v: " w" + i });
    many.push(" w" + i);
  }
  lines.push("[DONE]");
  const s2 = runSSE(P.makeChatGPTAccumulator(), dataLines(lines));
  eq("chatgpt 20 sticky deltas all captured", s2.text, many.join(""));

  // Snapshot-only short response (no appends) → finalize falls back to the
  // snapshot parts.
  const s3 = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      {
        p: "",
        o: "add",
        v: {
          message: {
            id: "m2",
            author: { role: "assistant" },
            content: { parts: ["Full answer here"] },
          },
          conversation_id: "c2",
        },
      },
      "[DONE]",
    ])
  );
  eq("chatgpt snapshot-only fallback", s3.text, "Full answer here");

  // stream_handoff (Pro/Thinking second-leg) sets the flag + TODO hook.
  const s4 = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([{ type: "stream_handoff", conversation_id: "c3" }, "[DONE]"])
  );
  ok("chatgpt stream_handoff flagged", s4.handoff === true);
})();

// ============= ChatGPT two-leg (thinking) handoff → resume ==============
(function chatgptTwoLeg() {
  // (a) THINKING turn: leg 1 = request(prompt+model) + handoff-only response;
  // leg 2 = the resume stream carrying the o/p/v answer + model_slug echo +
  // assistant msg id. The merge must yield leg-1 prompt + leg-2 response +
  // leg-2 model_slug. Synthetic content, real structure (ground-truth brief).

  // Leg 1 REQUEST: the prompt + request model ride the initial POST body.
  const leg1Req = P.parseChatGPTRequest(
    JSON.stringify({
      action: "next",
      model: "gpt-5-6-thinking",
      messages: [
        {
          id: "m_u",
          author: { role: "user" },
          content: { content_type: "text", parts: ["explain quicksort"] },
        },
      ],
      parent_message_id: "pmid",
    })
  );
  eq("2leg leg1 request prompt", leg1Req.prompt, "explain quicksort");
  eq("2leg leg1 request model", leg1Req.model, "gpt-5-6-thinking");

  // Leg 1 RESPONSE: handoff preamble + [DONE], NO answer text.
  const leg1State = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      {
        type: "stream_handoff",
        conversation_id: "cid_think",
        turn_exchange_id: "tx_1",
        options: [{ type: "resume_sse_endpoint", topic_id: "conversation-turn-tx_1" }],
      },
      "[DONE]",
    ])
  );
  ok("2leg leg1 is handoff-pending", P.chatGPTIsHandoffPending(leg1State) === true);
  eq("2leg leg1 learned conversation id", leg1State.conversationId, "cid_think");
  eq("2leg leg1 has no answer text", leg1State.text, "");

  // Leg 2 RESPONSE (POST /f/conversation/resume): re-emits the handoff
  // preamble, then the real answer as classic o/p/v sticky frames with a
  // model_slug echo + assistant msg id + conversation_id snapshot.
  const leg2State = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      { type: "stream_handoff", conversation_id: "cid_think" },
      {
        p: "",
        o: "add",
        v: {
          message: {
            id: "asst_think",
            author: { role: "assistant" },
            content: { content_type: "text", parts: [""] },
            metadata: { model_slug: "gpt-5-6-thinking" },
          },
          conversation_id: "cid_think",
        },
      },
      { p: "/message/content/parts/0", o: "append", v: "Quicksort " },
      { v: "partitions" },
      { v: " around a pivot." },
      "[DONE]",
    ])
  );
  eq("2leg leg2 answer text", leg2State.text, "Quicksort partitions around a pivot.");
  eq("2leg leg2 model_slug echo", leg2State.model, "gpt-5-6-thinking");
  eq("2leg leg2 assistant msg id", leg2State.messageId, "asst_think");

  // MERGE: leg-1 prompt + leg-2 answer + leg-2 model (slug echo preferred).
  const merged = P.mergeChatGPTLegs(
    { prompt: leg1Req.prompt, model: leg1Req.model, conversationId: leg1State.conversationId },
    leg2State
  );
  eq("2leg merged prompt (from leg1)", merged.prompt, "explain quicksort");
  eq("2leg merged response (from leg2)", merged.text, "Quicksort partitions around a pivot.");
  eq("2leg merged model (slug echo preferred)", merged.model, "gpt-5-6-thinking");
  eq("2leg merged msg id (from leg2)", merged.messageId, "asst_think");
  eq("2leg merged conversation id", merged.conversationId, "cid_think");

  // resume-path predicate: matches both bases' /resume, never /prepare or base.
  ok("2leg resume path (/f/)", P.isChatGPTResumePath("/backend-api/f/conversation/resume") === true);
  ok("2leg resume path (non-/f/)", P.isChatGPTResumePath("/backend-api/conversation/resume") === true);
  ok("2leg base is not resume", P.isChatGPTResumePath("/backend-api/f/conversation") === false);
  ok("2leg prepare is not resume", P.isChatGPTResumePath("/backend-api/f/conversation/prepare") === false);

  // (b) NON-THINKING turn: o/p/v stream directly on leg 1, NO handoff. It must
  // NOT be flagged pending — a complete turn emits from leg 1 alone.
  const direct = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      {
        p: "",
        o: "add",
        v: {
          message: {
            id: "asst_fast",
            author: { role: "assistant" },
            content: { content_type: "text", parts: [""] },
            metadata: { model_slug: "gpt-5-6" },
          },
          conversation_id: "cid_fast",
        },
      },
      { p: "/message/content/parts/0", o: "append", v: "42" },
      { v: " is the answer." },
      "[DONE]",
    ])
  );
  ok("nonthink not handoff-pending", P.chatGPTIsHandoffPending(direct) === false);
  eq("nonthink complete text from leg1", direct.text, "42 is the answer.");
  eq("nonthink model_slug from leg1", direct.model, "gpt-5-6");
  eq("nonthink conversation id from leg1", direct.conversationId, "cid_fast");
})();

// ============================ Claude.ai =================================
(function claude() {
  const stream = dataLines([
    {
      type: "message_start",
      message: {
        id: "msg_claude",
        model: "claude-sonnet-4-5",
        usage: { input_tokens: 42 },
      },
    },
    { type: "content_block_start", index: 0, content_block: { type: "text" } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "Hi" } },
    // thinking_delta must NOT enter the visible response text.
    {
      type: "content_block_delta",
      index: 0,
      delta: { type: "thinking_delta", thinking: "SECRET REASONING" },
    },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: " there" } },
    { type: "content_block_stop", index: 0 },
    { type: "message_delta", delta: {}, usage: { output_tokens: 7 } },
    { type: "message_stop" },
  ]);
  const s = runSSE(P.makeClaudeAccumulator(), stream);
  eq("claude text_delta assembled", s.text, "Hi there");
  ok("claude thinking excluded", s.text.indexOf("SECRET") === -1);
  eq("claude model from message_start", s.model, "claude-sonnet-4-5");
  eq("claude message id", s.messageId, "msg_claude");
  eq("claude input_tokens usage", s.inputTokens, 42);
  eq("claude cumulative output_tokens", s.outputTokens, 7);

  // Request-body prompt/model extraction.
  const req = P.parseClaudeRequest(
    JSON.stringify({ prompt: "what is 2+2?", model: "claude-opus-4-5" })
  );
  eq("claude request prompt", req.prompt, "what is 2+2?");
  eq("claude request model", req.model, "claude-opus-4-5");

  // LIVE-CONFIRMED 2026-07-10 web shape: message_start WITHOUT usage, a
  // message_delta WITHOUT usage, plus a message_limit event — the consumer
  // /completion stream carries no token counts. Text + model + id must still
  // parse; tokens stay 0 (estimated downstream). Structure only.
  const liveShape = dataLines([
    {
      type: "message_start",
      message: {
        id: "chatcompl_synthetic",
        type: "message",
        role: "assistant",
        model: "claude-opus-4-8",
        parent_uuid: "puuid",
        uuid: "auuid",
      },
    },
    { type: "content_block_start", index: 0, content_block: { type: "text", text: "", citations: [] } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "I" } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "'ll help." } },
    { type: "content_block_stop", index: 0 },
    { type: "message_delta", delta: { stop_reason: "end_turn" } },
    { type: "message_limit", message_limit: { type: "within_limit" } },
    { type: "message_stop" },
  ]);
  const ls = runSSE(P.makeClaudeAccumulator(), liveShape);
  eq("claude live-shape text (no usage)", ls.text, "I'll help.");
  eq("claude live-shape model", ls.model, "claude-opus-4-8");
  eq("claude live-shape message id", ls.messageId, "chatcompl_synthetic");
  eq("claude live-shape input tokens absent → 0", ls.inputTokens, 0);
  eq("claude live-shape output tokens absent → 0", ls.outputTokens, 0);
})();

// ============================ Perplexity ================================
(function perplexity() {
  // TRIPLE-nested decode: data.text (string) → steps[] → FINAL.content
  // (string) → { answer }.
  const finalObj = { answer: "The capital of France is Paris.", chunks: [] };
  const steps = [
    { step_type: "SEARCH_RESULTS", content: "irrelevant" },
    { step_type: "FINAL", content: JSON.stringify(finalObj) },
  ];
  const frame = {
    backend_uuid: "bk_1",
    uuid: "u_1",
    text: JSON.stringify(steps),
  };
  const s = runSSE(P.makePerplexityAccumulator(), dataLines([frame]));
  eq("perplexity triple-decode answer", s.text, "The capital of France is Paris.");
  eq("perplexity conversation id (backend_uuid)", s.conversationId, "bk_1");
  eq("perplexity message id (uuid)", s.messageId, "u_1");

  // Schematized-API build (LIVE-CONFIRMED 2026-07-10, pplx_pro_upgraded +
  // params.use_schematized_api=true): FINAL.content arrives as an OBJECT
  // directly and its `.answer` is a NESTED JSON string `{"answer":"…"}` — a
  // single `.answer` read would return the opaque wrapper, so the parser peels
  // it (unwrapSchematizedAnswer). Structure only; content is synthetic.
  const schemInner = JSON.stringify({ answer: "Louis XVI was the last king.", chunks: [] });
  const schemFrame = {
    backend_uuid: "bk_s",
    uuid: "u_s",
    text: JSON.stringify([
      { step_type: "INITIAL_QUERY", content: "irrelevant" },
      { step_type: "FINAL", content: { answer: schemInner, chunks: [] } },
    ]),
  };
  const ss = runSSE(P.makePerplexityAccumulator(), dataLines([schemFrame]));
  eq("perplexity schematized double-wrap unwrapped", ss.text, "Louis XVI was the last king.");
  eq("perplexity schematized backend_uuid", ss.conversationId, "bk_s");

  // Cumulative snapshots — keep the longest FINAL answer.
  const grow = dataLines([
    { text: JSON.stringify([{ step_type: "FINAL", content: JSON.stringify({ answer: "Paris" }) }]) },
    { text: JSON.stringify([{ step_type: "FINAL", content: JSON.stringify({ answer: "Paris is the capital." }) }]) },
  ]);
  eq("perplexity keeps longest FINAL", runSSE(P.makePerplexityAccumulator(), grow).text, "Paris is the capital.");

  // Fallback: an older plain-answer shape still works.
  const s2 = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([{ answer: "plain answer", backend_uuid: "bk_2" }])
  );
  eq("perplexity plain-answer fallback", s2.text, "plain answer");

  // Request parse: query_str + params.model_preference.
  const req = P.parsePerplexityRequest(
    JSON.stringify({ query_str: "hello?", params: { model_preference: "sonar-pro", mode: "concise" } })
  );
  eq("perplexity request prompt", req.prompt, "hello?");
  eq("perplexity request model (params.model_preference)", req.model, "sonar-pro");
})();

// ============================ Gemini ====================================
(function gemini() {
  // Build one batchexecute frame with the documented candidate layout, then
  // wrap it in the )]}' + UTF-16-length-prefix envelope.
  function envelope(frameArr) {
    const frameStr = JSON.stringify(frameArr);
    // LENGTH is UTF-16 code units — native JS .length. A byte-based parser
    // would miscount when the payload holds non-BMP characters.
    return ")]}'\n\n" + frameStr.length + "\n" + frameStr + "\n";
  }
  function wrbEntry(partJson) {
    return ["wrb.fr", "abc123", JSON.stringify(partJson), null, null, null, "generic"];
  }
  function candidate(rcid, text, done) {
    const c = [rcid, [text]];
    c[8] = [done ? 2 : 1];
    return c;
  }
  // part_json: [_, [cid, rid], _, _, [candidate...]]
  function partJson(cid, rid, cands) {
    return [null, [cid, rid], null, null, cands];
  }

  // Snapshot frames: Gemini re-sends the FULL cumulative text each frame.
  // Include a non-BMP emoji to prove UTF-16 code-unit slicing.
  const f1 = envelope([wrbEntry(partJson("cid_9", "rid_1", [candidate("rc_1", "Hel", false)]))]);
  const f2 = envelope([
    wrbEntry(partJson("cid_9", "rid_1", [candidate("rc_1", "Hello 😀 world final", true)])),
  ]);
  const acc = P.makeGeminiAccumulator();
  acc.feed(f1);
  acc.feed(f2);
  acc.finalize();
  eq("gemini snapshot longest text (candidate[1][0])", acc.state.text, "Hello 😀 world final");
  eq("gemini conversation id (cid)", acc.state.conversationId, "cid_9");
  eq("gemini response id (rid)", acc.state.messageId, "rid_1");

  // Fallback text path candidate[22][0] when candidate[1] is absent.
  function candidate22(rcid, text) {
    const c = [rcid];
    c[22] = [text];
    return c;
  }
  const f3 = envelope([wrbEntry(partJson("cid_x", "rid_x", [candidate22("rc_2", "fallback path text")]))]);
  const acc2 = P.makeGeminiAccumulator();
  acc2.feed(f3);
  acc2.finalize();
  eq("gemini candidate[22][0] fallback", acc2.state.text, "fallback path text");

  // A malformed / non-envelope payload must not throw and yields empty text.
  const acc3 = P.makeGeminiAccumulator();
  acc3.feed("not a batchexecute payload at all");
  acc3.finalize();
  eq("gemini malformed payload → empty", acc3.state.text, "");

  // (c) Request prompt extraction from the form-encoded f.req (LIVE-CONFIRMED
  // 2026-07-11). Body = `f.req=<url-encoded JSON>&at=…`; f.req decodes to
  // `[null,"<inner-json-string>"]` and inner[0][0] is the prompt. Structure
  // only — synthetic prompt.
  const innerJson = JSON.stringify([["what is a monad?"], null, null]);
  const fReq = JSON.stringify([null, innerJson]);
  const geminiBody =
    "f.req=" + encodeURIComponent(fReq) + "&at=" + encodeURIComponent("anti-forgery-tok") + "&";
  const greq = P.parseGeminiRequest(geminiBody);
  eq("gemini request prompt from f.req", greq.prompt, "what is a monad?");
  eq("gemini request model stays empty (DOM read)", greq.model, "");

  // Defensive: a malformed f.req must fail soft to an empty prompt, not throw.
  eq(
    "gemini request malformed f.req → empty",
    P.parseGeminiRequest("f.req=%7Bnot-json&at=x").prompt,
    ""
  );
  eq("gemini request no f.req → empty", P.parseGeminiRequest("other=1").prompt, "");
  eq("gemini request empty body → empty", P.parseGeminiRequest("").prompt, "");
})();

// ============================ ChatGPT request ===========================
(function chatgptRequest() {
  const req = P.parseChatGPTRequest(
    JSON.stringify({
      model: "gpt-5-6",
      conversation_id: "conv_r",
      messages: [
        { author: { role: "system" }, content: { parts: ["sys"] } },
        { author: { role: "user" }, content: { parts: ["what is the time?"] } },
      ],
    })
  );
  eq("chatgpt request model", req.model, "gpt-5-6");
  eq("chatgpt request conversation id", req.conversationId, "conv_r");
  eq("chatgpt request last-user prompt", req.prompt, "what is the time?");
})();

console.log("parsers.test.js: " + passed + " assertions passed");
