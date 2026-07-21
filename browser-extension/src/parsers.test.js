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
  // the #1 silent-failure mode a require-o+p parser drops. The answer-container
  // snapshot precedes the appends (as the real wire always does) so the
  // wave-5 answer-scoped harvest gate is satisfied — this exercises the
  // sticky-frame parser, not the container gate.
  const many = ["First"];
  const lines = [
    {
      p: "",
      o: "add",
      v: {
        message: {
          id: "msg_sticky",
          author: { role: "assistant" },
          content: { content_type: "text", parts: [""] },
        },
        conversation_id: "conv_sticky",
      },
    },
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

// ========== ChatGPT FREE-TIER inline-SSE turn (2026-07-18) ==============
// LIVE-diagnosed regression: a FREE-tier turn (plan_type:"free",
// resolved_model_slug:"gpt-5-5", fast_convo:true) streams the FULL answer
// INLINE on the leg-1 POST /backend-api/f/conversation SSE response as
// delta_encoding="v1" patches — with NO ws.chatgpt.com handoff — EVEN THOUGH a
// `resume_conversation_token` (kind:topic) is present at the top (the server
// supports ws-resume but chose inline for this fast turn). The answer wire
// shape + event order below mirror the real fixture
// (scratchpad/chatgpt-freetier-inline-sse-2026-07-18.md). The bug: the leg-1
// SSE begins with an `event: delta_encoding` / `data: "v1"` marker that parses
// to the STRING "v1" — the SSE accumulator's `"o" in ev` test THREW on it,
// aborting the whole feed so the inline answer was lost (free-tier captured
// nothing on any OS). Fixed by dropping non-object frames like the WS path.
(function chatgptFreeTierInlineSSE() {
  const CONV = "6a5b9619-b8a4-83ee-a940-bae8d3a35530";
  const TX = "0dcf1818-6c17-4bba-89fe-5a7881601b95"; // turn_exchange/topic id
  const MODEL = "gpt-5-5";
  const PROMPT = "This is a test chatgpt message";
  const ANSWER =
    "Test received! ✅\n\nYour message came through successfully. How can I help you today?";

  // base64url-encode a synthetic resume_conversation_token JWT so
  // turnTopicFromJWT can decode turn_topic_id → the normalized exchange id.
  const b64u = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
  const resumeJWT =
    b64u({ alg: "HS256", typ: "JWT" }) +
    "." +
    b64u({ conduit_uuid: "cu", turn_topic_id: "conversation-turn-" + TX, exp: 1 }) +
    ".sig";

  // rawSSE builds a raw SSE body from a list of [eventName|null, dataObjOrStr]
  // pairs, faithfully emitting the `event:` marker lines the fixture carries.
  function rawSSE(pairs) {
    let s = "";
    for (const [ev, data] of pairs) {
      if (ev) s += "event: " + ev + "\n";
      s += "data: " + (typeof data === "string" ? data : JSON.stringify(data)) + "\n\n";
    }
    return s;
  }
  function feedSplit(acc, s) {
    const mid = Math.floor(s.length / 2);
    acc.feed(s.slice(0, mid));
    acc.feed(s.slice(mid));
    acc.finalize();
    return acc.state;
  }

  const asstMsg = {
    message: {
      id: "c289780f-9d6b-4f2f-b6a1-answer0001",
      author: { role: "assistant" },
      content: { content_type: "text", parts: [""] },
      status: "in_progress",
      channel: "final",
      metadata: { resolved_model_slug: MODEL },
    },
    conversation_id: CONV,
  };

  // The full free-tier inline turn, in fixture order.
  const freeTierStream = rawSSE([
    ["delta_encoding", '"v1"'], // <-- the marker that used to CRASH the SSE leg
    [null, { type: "resume_conversation_token", kind: "topic", token: resumeJWT, conversation_id: CONV }],
    // Hidden system messages (excluded), user echo (excluded), an assistant
    // model_editable_context message (assistant but NOT the answer container).
    [null, { p: "", o: "add", v: { message: { id: "sys", author: { role: "system" }, content: { content_type: "text", parts: [""] }, weight: 0, metadata: { is_visually_hidden_from_conversation: true } }, conversation_id: CONV } }],
    [null, { v: { message: { id: "u1", author: { role: "user" }, content: { content_type: "text", parts: [PROMPT] }, metadata: { resolved_model_slug: MODEL } }, conversation_id: CONV } }],
    [null, { type: "input_message", input_message: { author: { role: "user" }, content: { parts: [PROMPT] } } }],
    [null, { v: { message: { id: "ctx", author: { role: "assistant" }, content: { content_type: "model_editable_context", model_set_context: "" }, metadata: { resolved_model_slug: MODEL } }, conversation_id: CONV } }],
    // The REAL answer container (channel final / content_type text).
    [null, { v: asstMsg }],
    [null, { type: "message_marker", marker: "user_visible_token" }],
    // Explicit append, then THREE bare-{"v":"…"} continuations (o+p omitted).
    [null, { p: "/message/content/parts/0", o: "append", v: "Test received" }],
    [null, { v: "! ✅" }],
    [null, { v: "\n\nYour message came through" }],
    [null, { v: " successfully. How can I help" }],
    // Terminal patch batch: final append + status/end_turn/metadata replaces.
    [null, { p: "", o: "patch", v: [
      { p: "/message/content/parts/0", o: "append", v: " you today?" },
      { p: "/message/status", o: "replace", v: "finished_successfully" },
      { p: "/message/end_turn", o: "replace", v: true },
      { p: "/message/metadata", o: "append", v: { is_complete: true } },
    ] }],
    [null, { type: "message_stream_complete" }],
    [null, "[DONE]"],
  ]);

  // ---- (a) full free-tier inline turn harvests the complete answer ----
  const s = feedSplit(P.makeChatGPTAccumulator(), freeTierStream);
  eq("freetier full answer text (all bare-v chunks)", s.text, ANSWER);
  eq("freetier conversation id", s.conversationId, CONV);
  eq("freetier model gpt-5-5 (resolved_model_slug)", s.model, MODEL);
  eq("freetier assistant answer msg id", s.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");
  // The resume token does NOT flag a pending handoff on the SSE leg — the
  // answer arrived inline, so this leg EMITS (hasResponseContent gate) even
  // though a resume_conversation_token was present.
  ok("freetier handoff flag NOT set (no stream_handoff frame)", s.handoff === false);
  ok("freetier is NOT handoff-pending", P.chatGPTIsHandoffPending(s) === false);
  ok("freetier hasResponseContent → SSE leg emits", P.hasResponseContent(s) === true);
  // handoffId is still harvested (from the resume token JWT) as the
  // cross-transport dedup key so a paid turn that ALSO answers over WS dedups.
  eq("freetier handoffId harvested from resume token", s.handoffId, TX);

  // ---- (b) PAID contentless-handoff leg-1: NO inline answer → no emit ----
  // A thinking turn's leg-1 is a `stream_handoff` preamble (prefixed by the
  // SAME delta_encoding marker) with NO answer text — the WS carries the
  // answer. The SSE leg must harvest NOTHING and stay handoff-pending so
  // completeSSETurn's hasResponseContent gate suppresses it (WS path emits).
  const paidLeg1 = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { type: "resume_conversation_token", kind: "topic", token: resumeJWT, conversation_id: CONV }],
      [null, { type: "stream_handoff", conversation_id: CONV, turn_exchange_id: TX, options: [{ type: "resume_sse_endpoint", topic_id: "conversation-turn-" + TX }] }],
      [null, "[DONE]"],
    ])
  );
  eq("paid leg-1 harvests no answer text", paidLeg1.text, "");
  ok("paid leg-1 handoff flag set", paidLeg1.handoff === true);
  ok("paid leg-1 IS handoff-pending", P.chatGPTIsHandoffPending(paidLeg1) === true);
  ok("paid leg-1 hasResponseContent false → SSE leg suppressed", P.hasResponseContent(paidLeg1) === false);
  eq("paid leg-1 still learned handoffId (dedup key for WS)", paidLeg1.handoffId, TX);
  eq("paid leg-1 delta_encoding marker did not crash the feed", paidLeg1.conversationId, CONV);

  // ---- (c) bare-{"v":"…"} continuation append (o AND p omitted) ----
  // Unit coverage that the SSE accumulator's sticky-frame path appends a bare
  // {"v":"…"} to the LAST-touched path — the delta_encoding v1 shorthand the WS
  // accumulator documents; the SSE leg MUST mirror it or it harvests partial.
  const bare = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "A" }],
      [null, { v: "B" }], // bare-v continuation → same path
      [null, { v: "C" }], // bare-v continuation → same path
      [null, "[DONE]"],
    ])
  );
  eq("freetier bare-v continuation appends to last path", bare.text, "ABC");

  // ---- (d) cross-transport dedup: same turn on BOTH legs → single emit ----
  // A simple turn can (defense-in-depth) answer on BOTH the inline SSE leg AND
  // the ws.chatgpt.com socket. content-main.js keys the recentEmits dedup on
  // the SSE leg's state.handoffId and the WS leg's [messageId, workingTurnId,
  // handoffId]; a match on EITHER means whichever leg completes first wins and
  // the other dedups. This pins the PURE invariant that makes that work: the
  // SSE handoffId is present in the WS leg's key set (same turn).
  const WKG = "abcd1234-working-turn-id-0001";
  const sseLeg = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { type: "resume_conversation_token", kind: "topic", token: resumeJWT, conversation_id: CONV }],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
      [null, "[DONE]"],
    ])
  );
  // The WS leg of the SAME turn (turn_id == the exchange id TX).
  function wsFrame(payloadInner) {
    return JSON.stringify([
      { type: "message", topic_id: "conversation-turn-" + TX, payload: { type: "conversation-turn-stream", payload: payloadInner } },
    ]);
  }
  const wsMini = (data) => "data: " + JSON.stringify(data) + "\n\n";
  const wsLeg = P.makeChatGPTWSAccumulator();
  wsLeg.feed(wsFrame({ type: "stream-item", conversation_id: CONV, turn_id: TX, encoded_item: wsMini({ type: "resume_conversation_token", token: resumeJWT, conversation_id: CONV }) }));
  wsLeg.feed(wsFrame({ type: "stream-item", conversation_id: CONV, turn_id: TX, encoded_item: wsMini({ type: "input_message", input_message: { author: { role: "user" }, content: { parts: [PROMPT] }, metadata: { working_turn_id: WKG } } }) }));
  wsLeg.feed(wsFrame({ type: "stream-item", conversation_id: CONV, turn_id: TX, encoded_item: wsMini({ v: asstMsg }) }));
  wsLeg.feed(wsFrame({ type: "stream-item", conversation_id: CONV, turn_id: TX, encoded_item: wsMini({ p: "/message/content/parts/0", o: "append", v: "Hi" }) }));
  wsLeg.feed(wsFrame({ type: "done", conversation_id: CONV, turn_id: TX }));
  wsLeg.finalize();
  const ws = wsLeg.state;
  eq("dedup: SSE handoffId == WS turn/messageId", sseLeg.handoffId, ws.messageId);
  const wsKeys = [ws.messageId, ws.workingTurnId, ws.handoffId];
  ok("dedup: WS key set contains the SSE handoffId (single-emit)", wsKeys.indexOf(sseLeg.handoffId) !== -1);
  ok("dedup: WS also harvested the second working-turn-id key", ws.workingTurnId === WKG);
  eq("dedup: both legs harvested the same answer", sseLeg.text, ws.text);

  // ---- (e) SEMANTIC-COMPLETION gate (re-review HIGH #1) ----
  // A fully-streamed free-tier turn carries the terminal patch
  // (status:finished_successfully / end_turn:true / metadata is_complete) AND
  // the `message_stream_complete` event → state.complete true → the SSE leg
  // EMITS (preserving the just-shipped free-tier inline fix). A TRUNCATED
  // variant (same turn MINUS those terminal signals) still harvests partial
  // text but state.complete stays false → the transport's requireStreamComplete
  // gate SUPPRESSES the emit so it can't clobber the WS leg's complete answer
  // via recentEmits.
  ok("freetier complete turn sets state.complete (still emits)", s.complete === true);

  const truncated = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Test received" }],
      [null, { v: "! ✅" }],
      [null, { v: "\n\nYour message came through" }],
      // stream ERRORS/closes here — NO terminal patch, NO message_stream_complete.
    ])
  );
  ok("truncated turn still harvested partial text", truncated.text.length > 0);
  ok("truncated turn has response content (would emit without the gate)", P.hasResponseContent(truncated) === true);
  ok("truncated turn state.complete FALSE → SSE leg suppressed", truncated.complete === false);

  // Each terminal signal INDEPENDENTLY marks completion (robust to the exact
  // wire shape a given build emits).
  const onlyEndTurn = feedSplit(P.makeChatGPTAccumulator(), rawSSE([
    ["delta_encoding", '"v1"'],
    [null, { v: asstMsg }],
    [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
    [null, { p: "/message/end_turn", o: "replace", v: true }],
  ]));
  ok("end_turn:true alone marks complete", onlyEndTurn.complete === true);

  const onlyStreamComplete = feedSplit(P.makeChatGPTAccumulator(), rawSSE([
    ["delta_encoding", '"v1"'],
    [null, { v: asstMsg }],
    [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
    [null, { type: "message_stream_complete" }],
  ]));
  ok("message_stream_complete event alone marks complete", onlyStreamComplete.complete === true);

  const onlyStatus = feedSplit(P.makeChatGPTAccumulator(), rawSSE([
    ["delta_encoding", '"v1"'],
    [null, { v: asstMsg }],
    [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
    [null, { p: "", o: "patch", v: [{ p: "/message/status", o: "replace", v: "finished_successfully" }] }],
  ]));
  ok("status:finished_successfully (patch sub-op) marks complete", onlyStatus.complete === true);

  const onlyMetaObj = feedSplit(P.makeChatGPTAccumulator(), rawSSE([
    ["delta_encoding", '"v1"'],
    [null, { v: asstMsg }],
    [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
    [null, { p: "/message/metadata", o: "append", v: { is_complete: true } }],
  ]));
  ok("metadata object {is_complete:true} marks complete", onlyMetaObj.complete === true);

  // A mid-stream in_progress snapshot must NOT falsely mark complete.
  const inProgress = feedSplit(P.makeChatGPTAccumulator(), rawSSE([
    ["delta_encoding", '"v1"'],
    [null, { v: asstMsg }], // asstMsg.status == "in_progress"
    [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
  ]));
  ok("in_progress snapshot does NOT mark complete", inProgress.complete === false);

  // ---- (f) HIGH #1 (deeper): completion is ANSWER-SCOPED --------------------
  // The REAL free-tier SSE ECHOES the USER turn AND streams hidden system
  // messages that ALREADY carry status:"finished_successfully" BEFORE the
  // assistant answer streams (the trap: the answer c289780f… only reaches
  // finished_successfully in the TERMINAL patch). A completion signal from the
  // user echo / a hidden message must NOT flip state.complete — otherwise a
  // later TRUNCATED answer still passes requireStreamComplete and its partial
  // emit clobbers the WS leg's complete answer via recentEmits.

  // A hidden system message + the user echo, BOTH pre-carrying the terminal
  // status, then a TRUNCATED assistant answer (no terminal patch, no
  // message_stream_complete) → NOT complete (SSE leg suppressed).
  const trapTruncated = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { p: "", o: "add", v: { message: { id: "sys", author: { role: "system" }, content: { content_type: "text", parts: [""] }, weight: 0, status: "finished_successfully", metadata: { is_visually_hidden_from_conversation: true } }, conversation_id: CONV } }],
      [null, { v: { message: { id: "u1", author: { role: "user" }, content: { content_type: "text", parts: [PROMPT] }, status: "finished_successfully", metadata: { resolved_model_slug: MODEL } }, conversation_id: CONV } }],
      // The REAL answer container opens in_progress, streams partial, then CUTS.
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Test received" }],
      [null, { v: "! ✅" }],
      // stream ERRORS here — NO terminal patch / message_stream_complete.
    ])
  );
  ok("trap: user-echo/hidden finished_successfully does NOT mark complete", trapTruncated.complete === false);
  ok("trap: truncated answer still harvested partial text", trapTruncated.text.length > 0);
  eq("trap: answer id is the assistant answer, not the user echo", trapTruncated.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");
  ok("trap: user echo prompt excluded from answer text", trapTruncated.text.indexOf(PROMPT) === -1);

  // The SAME trap but the answer COMPLETES (terminal patch on the answer
  // container) → state.complete flips TRUE only now → the SSE leg emits. Proves
  // the answer-scoping does NOT regress the free-tier complete-turn emit even
  // when the user echo pre-carried the terminal status.
  const trapComplete = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: { message: { id: "u1", author: { role: "user" }, content: { content_type: "text", parts: [PROMPT] }, status: "finished_successfully" }, conversation_id: CONV } }],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
      [null, { p: "", o: "patch", v: [
        { p: "/message/status", o: "replace", v: "finished_successfully" },
        { p: "/message/end_turn", o: "replace", v: true },
      ] }],
      [null, { type: "message_stream_complete" }],
    ])
  );
  ok("trap-complete: answer terminal patch DOES mark complete", trapComplete.complete === true);
  eq("trap-complete: answer id from the answer container", trapComplete.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");
  eq("trap-complete: full text harvested", trapComplete.text, "Hi");

  // A model_editable_context message (author assistant, content_type
  // model_editable_context) carrying a terminal status is NOT the answer: it
  // must neither mark complete NOR become the harvested answer id, and a
  // FOLLOWING truncated real answer stays incomplete. (The model echo it
  // carries is still harvested — a model string is answer-agnostic.)
  const ctxTrap = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: { message: { id: "ctx", author: { role: "assistant" }, content: { content_type: "model_editable_context", model_set_context: "" }, status: "finished_successfully", metadata: { resolved_model_slug: MODEL } }, conversation_id: CONV } }],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "partial" }],
      // truncated — no terminal signal on the answer container.
    ])
  );
  ok("ctx-trap: model_editable_context status does NOT mark complete", ctxTrap.complete === false);
  eq("ctx-trap: answer id is NOT the context message", ctxTrap.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");
  eq("ctx-trap: model still harvested from the context message", ctxTrap.model, MODEL);

  // message_stream_complete with NO answer container ever seen (only a user
  // echo) must NOT mark complete — the event finalizes a real answer only.
  const streamCompleteNoAnswer = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: { message: { id: "u1", author: { role: "user" }, content: { content_type: "text", parts: [PROMPT] }, status: "finished_successfully" }, conversation_id: CONV } }],
      [null, { type: "message_stream_complete" }],
    ])
  );
  ok("stream_complete without an answer container does NOT mark complete", streamCompleteNoAnswer.complete === false);

  // ---- (g) HIGH #1 (DEFINITIVE, wave-5): isAnswerContainer rejects weight-0 AND
  // non-final-channel snapshots -----------------------------------------------
  // A weight-0 assistant/text snapshot (a hidden/system message that carries NO
  // explicit is_visually_hidden flag) that pre-carries a terminal status must
  // NOT become the answer container: it must neither mark complete NOR steal the
  // answer id, and a FOLLOWING truncated real answer stays incomplete. Mirrors
  // the WS answer predicate (weight===0 ≡ hidden).
  const weight0Trap = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      // weight-0 assistant TEXT snapshot, terminal status, NO hidden flag.
      [null, { v: { message: { id: "w0", author: { role: "assistant" }, content: { content_type: "text", parts: ["ghost"] }, weight: 0, status: "finished_successfully", channel: "final" }, conversation_id: CONV } }],
      // The REAL answer opens in_progress and then CUTS (truncated).
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Real partial" }],
      // stream errors — no terminal signal on the real answer.
    ])
  );
  ok("weight0-trap: weight-0 assistant snapshot does NOT mark complete", weight0Trap.complete === false);
  eq("weight0-trap: answer id is the real answer, not the weight-0 ghost", weight0Trap.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");
  ok("weight0-trap: weight-0 ghost text excluded from the answer", weight0Trap.text.indexOf("ghost") === -1);
  eq("weight0-trap: only the real answer text harvested", weight0Trap.text, "Real partial");

  // A channel present-but-NOT-"final" assistant text snapshot is not the answer
  // (an analysis/commentary channel). It must not become the answer id, must not
  // mark complete, and its parts must not be harvested; a following real
  // channel:"final" answer is the one that pins.
  const nonFinalChannel = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: { message: { id: "analysis", author: { role: "assistant" }, content: { content_type: "text", parts: ["thinking out loud"] }, channel: "analysis", status: "finished_successfully" }, conversation_id: CONV } }],
      [null, { p: "/message/content/parts/0", o: "append", v: "SCRATCH" }],
      [null, { v: asstMsg }], // the real channel:"final" answer
      [null, { p: "/message/content/parts/0", o: "append", v: "Final answer" }],
      [null, { p: "", o: "patch", v: [
        { p: "/message/status", o: "replace", v: "finished_successfully" },
        { p: "/message/end_turn", o: "replace", v: true },
      ] }],
      [null, { type: "message_stream_complete" }],
    ])
  );
  eq("non-final-channel: answer id is the channel:final message", nonFinalChannel.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");
  ok("non-final-channel: analysis-channel text excluded", nonFinalChannel.text.indexOf("SCRATCH") === -1);
  ok("non-final-channel: analysis-channel scratch excluded", nonFinalChannel.text.indexOf("thinking out loud") === -1);
  eq("non-final-channel: only the final-channel answer harvested", nonFinalChannel.text, "Final answer");
  ok("non-final-channel: turn still completes on the final answer", nonFinalChannel.complete === true);

  // The channel:"final" answer (fixture shape) IS the container even when a
  // channel field is present — confirms the guard is present-and-not-final, not
  // present-at-all.
  const channelFinal = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }], // asstMsg.channel === "final"
      [null, { p: "/message/content/parts/0", o: "append", v: "OK" }],
      [null, { p: "/message/end_turn", o: "replace", v: true }],
    ])
  );
  eq("channel:final IS the answer container", channelFinal.text, "OK");
  ok("channel:final answer marks complete", channelFinal.complete === true);

  // ---- (h) HIGH (wave-6): message_stream_complete is ANSWER-SCOPED, not ----
  // "an answer was ever seen" ------------------------------------------------
  // The DEFECT reproduction: the real answer opens, appends PARTIAL text, then
  // a TERMINAL non-answer snapshot (a model_editable_context / hidden / user
  // message) flips curMessageIsAnswer=false, and THEN an id-less
  // message_stream_complete arrives with NO terminal answer patch. The removed
  // ever-seen gate would have fired here → {text:"PARTIAL", complete:true} →
  // completeSSETurn emits the partial answer, clobbering a WS leg's complete
  // answer via recentEmits. Completion is now scoped to the ACTIVE answer, so
  // this stays incomplete (SSE leg suppressed).
  const streamCompleteAfterCtxSnapshot = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }], // the real answer container (in_progress)
      [null, { p: "/message/content/parts/0", o: "append", v: "PARTIAL" }],
      // A terminal model_editable_context snapshot flips curMessageIsAnswer off.
      [null, { v: { message: { id: "ctx", author: { role: "assistant" }, content: { content_type: "model_editable_context", model_set_context: "" }, status: "finished_successfully", metadata: { resolved_model_slug: MODEL } }, conversation_id: CONV } }],
      // Id-less turn-level completion — NOT scoped to the active answer.
      [null, { type: "message_stream_complete" }],
    ])
  );
  ok("wave-6: stream_complete after a non-answer snapshot does NOT mark complete", streamCompleteAfterCtxSnapshot.complete === false);
  eq("wave-6: partial answer text still harvested (would emit without the gate)", streamCompleteAfterCtxSnapshot.text, "PARTIAL");
  ok("wave-6: partial has response content (proves the gate, not empty-text, suppresses)", P.hasResponseContent(streamCompleteAfterCtxSnapshot) === true);
  eq("wave-6: answer id is the real answer container", streamCompleteAfterCtxSnapshot.messageId, "c289780f-9d6b-4f2f-b6a1-answer0001");

  // The SAME hostile ordering, but the answer received its TERMINAL PATCH
  // (status/end_turn, scoped through noteCompletion while it WAS the current
  // message) BEFORE the non-answer snapshot + id-less stream_complete. The
  // terminal patch alone completes the turn — proving the free-tier emit is
  // preserved even when message_stream_complete can no longer be correlated.
  const patchThenCtxThenStreamComplete = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Done" }],
      [null, { p: "", o: "patch", v: [
        { p: "/message/status", o: "replace", v: "finished_successfully" },
        { p: "/message/end_turn", o: "replace", v: true },
      ] }],
      // A trailing non-answer snapshot flips curMessageIsAnswer off AFTER
      // completion already latched on the answer message itself.
      [null, { v: { message: { id: "ctx", author: { role: "assistant" }, content: { content_type: "model_editable_context", model_set_context: "" }, metadata: { resolved_model_slug: MODEL } }, conversation_id: CONV } }],
      [null, { type: "message_stream_complete" }],
    ])
  );
  ok("wave-6: terminal answer patch alone still marks complete (free-tier emit preserved)", patchThenCtxThenStreamComplete.complete === true);
  eq("wave-6: complete turn text harvested", patchThenCtxThenStreamComplete.text, "Done");

  // The full free-tier fixture turn (already exercised as `s`) MUST still emit:
  // its terminal patch precedes message_stream_complete AND no intervening
  // non-answer snapshot flips curMessageIsAnswer, so both paths complete it.
  ok("wave-6: full free-tier turn STILL completes (fixture ordering)", s.complete === true);

  // An id-BEARING message_stream_complete whose id MATCHES the active answer
  // completes even if a later non-answer snapshot flipped curMessageIsAnswer —
  // a matching id is a reliable correlation (reviewer direction).
  const streamCompleteMatchingId = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
      [null, { v: { message: { id: "ctx", author: { role: "assistant" }, content: { content_type: "model_editable_context", model_set_context: "" } }, conversation_id: CONV } }],
      [null, { type: "message_stream_complete", message_id: "c289780f-9d6b-4f2f-b6a1-answer0001" }],
    ])
  );
  ok("wave-6: stream_complete with MATCHING message id completes across a non-answer snapshot", streamCompleteMatchingId.complete === true);

  // An id-BEARING message_stream_complete whose id does NOT match the active
  // answer is NOT a completion signal (can't be correlated → conservative).
  const streamCompleteMismatchedId = feedSplit(
    P.makeChatGPTAccumulator(),
    rawSSE([
      ["delta_encoding", '"v1"'],
      [null, { v: asstMsg }],
      [null, { p: "/message/content/parts/0", o: "append", v: "Hi" }],
      [null, { type: "message_stream_complete", message_id: "some-other-message-id" }],
    ])
  );
  ok("wave-6: stream_complete with a MISMATCHED message id does NOT mark complete", streamCompleteMismatchedId.complete === false);
})();

// ============ ChatGPT WebSocket answer stream (2026-07-17) ==============
// GROUNDED on live logged-in CDP recon: a thinking turn's ANSWER streams over
// the GLOBAL ws.chatgpt.com socket (topic conversation-turn-<turn_id>), NOT a
// /resume POST. Frames below use the REAL captured values (conversation id,
// turn id, model, prompt, answer) with the real frame structure.
(function chatgptWS() {
  const CONV = "6a5a5abe-60e8-83e8-8b58-bf47524f4cd3";
  const TURN = "e6a02413-1595-471d-a38a-57112b6cb7a2";
  const TOPIC = "conversation-turn-" + TURN;
  const PROMPT = "Reply with exactly the two words: FLUSH TEST";
  const MODEL = "gpt-5-6-thinking";

  // sse builds one encoded_item SSE mini-string ("event: X\ndata: {json}\n\n").
  function sse(eventName, data) {
    const body = typeof data === "string" ? data : JSON.stringify(data);
    return (eventName ? "event: " + eventName + "\n" : "") + "data: " + body + "\n\n";
  }
  // A WS text frame is a JSON ARRAY of message objects.
  function streamItemFrame(encoded) {
    return JSON.stringify([
      {
        type: "message",
        topic_id: TOPIC,
        payload: {
          type: "conversation-turn-stream",
          payload: {
            type: "stream-item",
            conversation_id: CONV,
            turn_id: TURN,
            encoded_item: encoded,
          },
        },
      },
    ]);
  }
  function doneFrame() {
    return JSON.stringify([
      {
        type: "message",
        topic_id: TOPIC,
        payload: {
          type: "conversation-turn-stream",
          payload: { type: "done", conversation_id: CONV, turn_id: TURN },
        },
      },
    ]);
  }
  function turnCompleteFrame() {
    return JSON.stringify([
      {
        type: "message",
        topic_id: "conversations",
        payload: {
          type: "conversation-turn-complete",
          payload: { conversation_id: CONV },
        },
      },
    ]);
  }
  // A subscribe reply with catchups[] (subscribe-recovery) — the turn boundary.
  function subscribeCatchupsFrame(encodedItems) {
    return JSON.stringify([
      {
        id: 5,
        type: "reply",
        reply: {
          type: "subscribe",
          topic_id: TOPIC,
          recovered: true,
          catchups: encodedItems.map((enc) => ({
            type: "message",
            topic_id: TOPIC,
            payload: {
              type: "conversation-turn-stream",
              payload: {
                type: "stream-item",
                conversation_id: CONV,
                turn_id: TURN,
                encoded_item: enc,
              },
            },
            offset: "1784306370324-0",
          })),
        },
      },
    ]);
  }

  const hiddenSystemMsg = {
    message: {
      id: "sys_hidden",
      author: { role: "system" },
      content: { content_type: "text", parts: [""] },
      weight: 0.0,
      metadata: { is_visually_hidden_from_conversation: true },
    },
    conversation_id: CONV,
  };
  const userMsg = {
    message: {
      id: "u1",
      author: { role: "user" },
      content: { content_type: "text", parts: [PROMPT] },
      weight: 1.0,
      metadata: { resolved_model_slug: MODEL },
    },
    conversation_id: CONV,
  };
  const inputMessage = {
    type: "input_message",
    input_message: {
      id: "u1",
      author: { role: "user" },
      content: { content_type: "text", parts: [PROMPT] },
      metadata: { resolved_model_slug: MODEL },
    },
    conversation_id: CONV,
  };
  // Assistant reasoning_recap (content.content, NOT parts) — NOT the answer.
  const reasoningRecap = {
    message: {
      id: "asst_recap",
      author: { role: "assistant" },
      content: { content_type: "reasoning_recap", content: "Worked for a couple of seconds" },
    },
    conversation_id: CONV,
  };
  // Assistant model_editable_context (no parts) — NOT the answer, carries model.
  const modelEditable = {
    message: {
      id: "asst_ctx",
      author: { role: "assistant" },
      content: { content_type: "model_editable_context", model_set_context: "" },
      metadata: { resolved_model_slug: MODEL },
    },
    conversation_id: CONV,
  };
  // The real assistant answer message (parts empty at stream start, channel final).
  const asstFinal = {
    message: {
      id: "90bf66ea-5953-4f0c-a888-9ddc1338ac7b",
      author: { role: "assistant" },
      content: { content_type: "text", parts: [""] },
      status: "in_progress",
      channel: "final",
    },
    conversation_id: CONV,
  };

  // ---- (a) FULL happy path, mirroring the real fixture flow ----
  const frames = [
    // subscribe + catchups: delta_encoding marker, resume token, stream_handoff.
    subscribeCatchupsFrame([
      sse("delta_encoding", '"v1"'),
      sse(null, { type: "resume_conversation_token", conversation_id: CONV, token: "x.y.z" }),
      sse(null, { type: "stream_handoff", conversation_id: CONV, turn_exchange_id: "bf89" }),
    ]),
    streamItemFrame(sse("delta_encoding", '"v1"')),
    streamItemFrame(sse("delta", { p: "", o: "add", v: hiddenSystemMsg, c: 0 })),
    // user message ADD (sticky: inherits o="add" p="" — o/p omitted).
    streamItemFrame(sse("delta", { v: userMsg, c: 3 })),
    streamItemFrame(sse(null, inputMessage)), // the input_message echo (plain data)
    streamItemFrame(sse("delta", { v: modelEditable, c: 4 })),
    streamItemFrame(sse(null, { type: "title_generation", title: "Flush Test", conversation_id: CONV })),
    streamItemFrame(sse(null, { type: "message_marker", marker: "cot_token", event: "first" })),
    streamItemFrame(sse("delta", { v: reasoningRecap, c: 6 })),
    streamItemFrame(sse("delta", { v: asstFinal, c: 7 })),
    // The ANSWER: a multi-op patch batch (append text + status/end_turn replace).
    streamItemFrame(
      sse("delta", {
        o: "patch",
        v: [
          { p: "/message/content/parts/0", o: "append", v: "FLUSH TEST" },
          { p: "/message/status", o: "replace", v: "finished_successfully" },
          { p: "/message/end_turn", o: "replace", v: true },
        ],
      })
    ),
    streamItemFrame(sse(null, { type: "message_stream_complete", conversation_id: CONV })),
    streamItemFrame(sse(null, "[DONE]")),
    doneFrame(),
    turnCompleteFrame(),
  ];
  const acc = P.makeChatGPTWSAccumulator();
  let turnStarts = 0;
  for (const fr of frames) {
    if (P.chatGPTWSFrameIsTurnStart(fr)) turnStarts++;
    acc.feed(fr);
  }
  acc.finalize();
  const s = acc.state;
  eq("ws prompt from input_message/user echo", s.prompt, PROMPT);
  eq("ws final answer text (multi-op patch append)", s.text, "FLUSH TEST");
  eq("ws conversation id", s.conversationId, CONV);
  eq("ws messageId === turn_id", s.messageId, TURN);
  eq("ws model (resolved_model_slug)", s.model, MODEL);
  ok("ws done true after done frame", s.done === true);
  ok("ws answer excludes reasoning_recap", s.text.indexOf("Worked for") === -1);
  eq("ws exactly one turn-start (subscribe reply)", turnStarts, 1);

  // ---- (b) delta_encoding marker is ignored (no crash, no text) ----
  const marker = P.makeChatGPTWSAccumulator();
  marker.feed(streamItemFrame(sse("delta_encoding", '"v1"')));
  marker.finalize();
  eq("ws delta_encoding marker yields no text", marker.state.text, "");

  // ---- (c) hidden system weight-0 message is not counted as answer ----
  const hid = P.makeChatGPTWSAccumulator();
  hid.feed(streamItemFrame(sse("delta", { p: "", o: "add", v: hiddenSystemMsg, c: 0 })));
  hid.finalize();
  eq("ws hidden system message not answer text", hid.state.text, "");

  // ---- (d) single-op append shorthand + a bare {"v":"text"} continuation ----
  const cont = P.makeChatGPTWSAccumulator();
  cont.feed(streamItemFrame(sse("delta", { v: asstFinal, c: 0 }))); // establish assistant msg
  cont.feed(
    streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "Hello", c: 1 }))
  );
  cont.feed(streamItemFrame(sse("delta", { v: ", world" }))); // bare v — sticky path append
  cont.feed(streamItemFrame(sse("delta", { v: "!" })));
  cont.feed(doneFrame());
  cont.finalize();
  eq("ws single-op append + bare-v continuations", cont.state.text, "Hello, world!");

  // ---- (e) catchups[] walked: conversation id harvested from a subscribe ----
  const cu = P.makeChatGPTWSAccumulator();
  cu.feed(subscribeCatchupsFrame([sse(null, { type: "stream_handoff", conversation_id: CONV })]));
  eq("ws catchups[] conversation id harvested", cu.state.conversationId, CONV);

  // ---- (f) snapshot-only short answer (no append ops) → parts fallback ----
  const snap = P.makeChatGPTWSAccumulator();
  snap.feed(
    streamItemFrame(
      sse("delta", {
        p: "",
        o: "add",
        v: {
          message: {
            id: "asst_snap",
            author: { role: "assistant" },
            content: { content_type: "text", parts: ["Full answer here"] },
          },
          conversation_id: CONV,
        },
      })
    )
  );
  snap.feed(doneFrame());
  snap.finalize();
  eq("ws snapshot-only parts fallback", snap.state.text, "Full answer here");

  // ---- (g) conversation-turn-complete is a secondary done signal ----
  const secondary = P.makeChatGPTWSAccumulator();
  secondary.feed(streamItemFrame(sse("delta", { v: asstFinal, c: 0 })));
  secondary.feed(
    streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "Hi", c: 1 }))
  );
  secondary.feed(turnCompleteFrame()); // no explicit done frame — the complete is enough
  secondary.finalize();
  ok("ws conversation-turn-complete marks done", secondary.state.done === true);
  eq("ws secondary-complete still has answer text", secondary.state.text, "Hi");

  // ---- (h) chatGPTWSFrameIsTurnStart: only a subscribe reply on a turn topic ----
  ok("turn-start true for subscribe reply", P.chatGPTWSFrameIsTurnStart(subscribeCatchupsFrame([])) === true);
  ok("turn-start false for a stream-item frame", P.chatGPTWSFrameIsTurnStart(streamItemFrame(sse(null, "[DONE]"))) === false);
  ok("turn-start false for the done frame", P.chatGPTWSFrameIsTurnStart(doneFrame()) === false);
  ok(
    "turn-start false for an unsubscribe reply",
    P.chatGPTWSFrameIsTurnStart(JSON.stringify([{ id: 6, type: "reply", reply: { type: "unsubscribe", topic_id: TOPIC } }])) === false
  );
  ok("turn-start false for malformed frame", P.chatGPTWSFrameIsTurnStart("not json") === false);

  // ---- (i) multi-turn on ONE socket: a second subscribe resets state ----
  // The transport (content-main) resets a fresh accumulator per turn boundary;
  // assert the boundary detector fires for BOTH subscribes so the transport
  // knows to reset (a single accumulator is intentionally single-turn scoped).
  const TURN2 = "22222222-2222-4222-8222-222222222222";
  const frame2Sub = JSON.stringify([
    { id: 7, type: "reply", reply: { type: "subscribe", topic_id: "conversation-turn-" + TURN2, catchups: [] } },
  ]);
  ok("turn-start true for the 2nd turn subscribe", P.chatGPTWSFrameIsTurnStart(frame2Sub) === true);

  // ---- (j) MED: append-target tracking (hidden/system snapshots not counted) ----
  const hiddenAsst = {
    message: {
      id: "h1",
      author: { role: "assistant" },
      content: { content_type: "text", parts: ["HIDDEN SHOULD NOT COUNT"] },
      weight: 0.0,
      metadata: { is_visually_hidden_from_conversation: true },
    },
    conversation_id: CONV,
  };
  // A hidden weight-0 assistant TEXT snapshot alone is not the answer.
  const th = P.makeChatGPTWSAccumulator();
  th.feed(streamItemFrame(sse("delta", { p: "", o: "add", v: hiddenAsst, c: 0 })));
  th.feed(doneFrame());
  th.finalize();
  eq("ws hidden weight-0 assistant snapshot not counted", th.state.text, "");

  // Hidden assistant snapshot THEN parts/0 appends → appends NOT counted.
  const thp = P.makeChatGPTWSAccumulator();
  thp.feed(streamItemFrame(sse("delta", { p: "", o: "add", v: hiddenAsst, c: 0 })));
  thp.feed(streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "POISON", c: 1 })));
  thp.feed(doneFrame());
  thp.finalize();
  eq("ws appends after hidden target not counted", thp.state.text, "");

  // System snapshot then appends → not counted.
  const sysAppend = P.makeChatGPTWSAccumulator();
  sysAppend.feed(
    streamItemFrame(
      sse("delta", {
        p: "",
        o: "add",
        v: {
          message: {
            id: "s1",
            author: { role: "system" },
            content: { content_type: "text", parts: [""] },
            weight: 0.0,
            metadata: { is_visually_hidden_from_conversation: true },
          },
          conversation_id: CONV,
        },
        c: 0,
      })
    )
  );
  sysAppend.feed(streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "POISON", c: 1 })));
  sysAppend.feed(doneFrame());
  sysAppend.finalize();
  eq("ws appends after system target not counted", sysAppend.state.text, "");

  // Visible assistant message + appends → counted.
  const vis = P.makeChatGPTWSAccumulator();
  vis.feed(streamItemFrame(sse("delta", { v: asstFinal, c: 0 })));
  vis.feed(streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "ok", c: 1 })));
  vis.feed(doneFrame());
  vis.finalize();
  eq("ws appends after visible assistant target counted", vis.state.text, "ok");

  // Target flips to a hidden message after a real answer → trailing appends ignored.
  const flip = P.makeChatGPTWSAccumulator();
  flip.feed(streamItemFrame(sse("delta", { v: asstFinal, c: 0 })));
  flip.feed(streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "Hi", c: 1 })));
  flip.feed(streamItemFrame(sse("delta", { p: "", o: "add", v: hiddenAsst, c: 2 })));
  flip.feed(streamItemFrame(sse("delta", { p: "/message/content/parts/0", o: "append", v: "POISON", c: 3 })));
  flip.feed(doneFrame());
  flip.finalize();
  eq("ws append after target flips to hidden is ignored", flip.state.text, "Hi");

  // ---- (k) MED: id-only done (no prompt, no text) is not a real capture ----
  const idOnly = P.makeChatGPTWSAccumulator();
  idOnly.feed(doneFrame()); // ONLY a done frame — carries turn_id + conversation_id.
  idOnly.finalize();
  eq("ws id-only done still learns turn_id", idOnly.state.messageId, TURN);
  eq("ws id-only done still learns conversation_id", idOnly.state.conversationId, CONV);
  ok("ws id-only done has no prompt", idOnly.state.prompt === "");
  ok("ws id-only done has no text", idOnly.state.text === "");
  ok(
    "ws id-only done → tap skips (no prompt AND no text)",
    !idOnly.state.prompt && !idOnly.state.text
  );
})();

// ====== ChatGPT WS routing + overlapping-turn isolation (HIGH fix) =======
// chatGPTWSFrameRoute + a per-topic tap harness that MIRRORS content-main's
// attachWSAnswerTap Map routing — proving two turns interleaved on ONE socket
// stay fully independent (no cross-contamination), plus the Fix-2 SSE-gate keys.
(function chatgptWSRouting() {
  const CONV_A = "conv-aaaa";
  const CONV_B = "conv-bbbb";
  const TURN_A = "11111111-1111-4111-8111-111111111111";
  const TURN_B = "22222222-2222-4222-8222-222222222222";

  function sse(eventName, data) {
    const body = typeof data === "string" ? data : JSON.stringify(data);
    return (eventName ? "event: " + eventName + "\n" : "") + "data: " + body + "\n\n";
  }
  function frame(conv, turn, encoded) {
    return JSON.stringify([
      {
        type: "message",
        topic_id: "conversation-turn-" + turn,
        payload: {
          type: "conversation-turn-stream",
          payload: { type: "stream-item", conversation_id: conv, turn_id: turn, encoded_item: encoded },
        },
      },
    ]);
  }
  function subscribe(turn) {
    return JSON.stringify([
      { id: 1, type: "reply", reply: { type: "subscribe", topic_id: "conversation-turn-" + turn, catchups: [] } },
    ]);
  }
  function done(conv, turn) {
    return JSON.stringify([
      {
        type: "message",
        topic_id: "conversation-turn-" + turn,
        payload: { type: "conversation-turn-stream", payload: { type: "done", conversation_id: conv, turn_id: turn } },
      },
    ]);
  }
  function asst(conv, id) {
    return sse("delta", {
      v: { message: { id, author: { role: "assistant" }, content: { content_type: "text", parts: [""] } }, conversation_id: conv },
      c: 0,
    });
  }
  function append(text) {
    return sse("delta", { p: "/message/content/parts/0", o: "append", v: text });
  }

  // route helper coverage.
  eq(
    "route subscribe → turnStart+topicId",
    JSON.stringify(P.chatGPTWSFrameRoute(subscribe(TURN_A))),
    JSON.stringify({ turnStart: true, topicId: TURN_A, completeConvId: "" })
  );
  eq("route stream-item → topicId", P.chatGPTWSFrameRoute(frame(CONV_A, TURN_A, asst(CONV_A, "a1"))).topicId, TURN_A);
  eq("route done → topicId", P.chatGPTWSFrameRoute(done(CONV_A, TURN_A)).topicId, TURN_A);
  eq(
    "route conversations-complete → completeConvId",
    P.chatGPTWSFrameRoute(
      JSON.stringify([{ type: "message", topic_id: "conversations", payload: { type: "conversation-turn-complete", payload: { conversation_id: CONV_A } } }])
    ).completeConvId,
    CONV_A
  );
  eq(
    "route unsubscribe → empty topic",
    P.chatGPTWSFrameRoute(JSON.stringify([{ id: 2, type: "reply", reply: { type: "unsubscribe", topic_id: "conversation-turn-" + TURN_A } }])).topicId,
    ""
  );
  eq("route malformed → empty", P.chatGPTWSFrameRoute("nope").topicId, "");

  // Per-topic tap harness — same shape as content-main attachWSAnswerTap.
  // runTap MIRRORS content-main's attachWSAnswerTap: PER-ELEMENT split routing
  // (chatGPTWSSplitFrame), per-topic accumulator Map, and the text-gated
  // conversation-complete correlation.
  function runTap(frames) {
    const emits = [];
    const turns = new Map();
    const make = P.makeChatGPTWSAccumulator;
    function close(tid, e) {
      if (e.acc.finalize) e.acc.finalize();
      const st = e.acc.state;
      turns.delete(tid);
      if (st.prompt || st.text)
        emits.push({
          messageId: st.messageId,
          text: st.text,
          conv: st.conversationId,
          handoffId: st.handoffId,
          workingTurnId: st.workingTurnId,
        });
    }
    for (const data of frames) {
      for (const part of P.chatGPTWSSplitFrame(data)) {
        if (part.topicId) {
          if (part.turnStart) turns.delete(part.topicId);
          let e = turns.get(part.topicId);
          if (!e) {
            e = { acc: make() };
            turns.set(part.topicId, e);
          }
          e.acc.feed(part.frame);
          if (e.acc.state.done) close(part.topicId, e);
        } else if (part.completeConvId) {
          let match = "";
          let count = 0;
          for (const [tid, e] of turns) {
            if (e.acc.state.conversationId === part.completeConvId) {
              match = tid;
              count++;
            }
          }
          // Text-gated: only close a single matching pending turn that ALREADY
          // has answer text (never a fresh/empty pending turn).
          if (count === 1 && turns.get(match).acc.state.text) close(match, turns.get(match));
        }
      }
    }
    return emits;
  }

  // INTERLEAVED turns A + B arriving mixed on ONE socket.
  const emits = runTap([
    subscribe(TURN_A),
    subscribe(TURN_B),
    frame(CONV_A, TURN_A, asst(CONV_A, "a1")),
    frame(CONV_B, TURN_B, asst(CONV_B, "b1")),
    frame(CONV_A, TURN_A, append("Alpha")),
    frame(CONV_B, TURN_B, append("Bravo")),
    frame(CONV_A, TURN_A, append(" answer")),
    frame(CONV_B, TURN_B, append(" reply")),
    done(CONV_B, TURN_B),
    done(CONV_A, TURN_A),
  ]);
  eq("interleaved: exactly two emits", emits.length, 2);
  const byTurn = {};
  for (const e of emits) byTurn[e.messageId] = e;
  eq("interleaved: A text isolated", byTurn[TURN_A].text, "Alpha answer");
  eq("interleaved: B text isolated", byTurn[TURN_B].text, "Bravo reply");
  eq("interleaved: A conv isolated", byTurn[TURN_A].conv, CONV_A);
  eq("interleaved: B conv isolated", byTurn[TURN_B].conv, CONV_B);
  eq("interleaved: no A text leaked into B", byTurn[TURN_B].text.indexOf("Alpha"), -1);
  eq("interleaved: no B text leaked into A", byTurn[TURN_A].text.indexOf("Bravo"), -1);

  // A secondary conversations-complete on the "conversations" topic AFTER both
  // turns emitted must NOT produce a spurious emit (both evicted → 0 matches).
  const trailing = runTap([
    subscribe(TURN_A),
    frame(CONV_A, TURN_A, asst(CONV_A, "a1")),
    frame(CONV_A, TURN_A, append("Done")),
    done(CONV_A, TURN_A),
    JSON.stringify([{ type: "message", topic_id: "conversations", payload: { type: "conversation-turn-complete", payload: { conversation_id: CONV_A } } }]),
  ]);
  eq("trailing complete after done → still exactly one emit", trailing.length, 1);
  eq("trailing: emitted text intact", trailing[0].text, "Done");

  // Fix 2 (SSE-leg gate + dedup key): a leg-1 handoff-only SSE stream has NO
  // response content (the SSE path must NOT emit it) yet DOES carry the
  // turn-unique handoffId that dedups against the WS emit (== turn_id).
  function dataLinesLocal(objs) {
    return objs.map((o) => "data: " + (typeof o === "string" ? o : JSON.stringify(o))).join("\n") + "\n";
  }
  const legAcc = P.makeChatGPTAccumulator();
  legAcc.feed(
    dataLinesLocal([
      { type: "stream_handoff", conversation_id: CONV_A, turn_exchange_id: TURN_A, options: [{ topic_id: "conversation-turn-" + TURN_A }] },
      "[DONE]",
    ])
  );
  legAcc.finalize();
  ok("Fix2: handoff-only SSE leg has no response content", P.hasResponseContent(legAcc.state) === false);
  eq("Fix2: SSE leg carries turn-unique handoffId (== turn_id, dedup key)", legAcc.state.handoffId, TURN_A);

  // ---- R2-1: ONE frame BATCHING two topics' stream-items → split isolates ----
  // chatGPTWSSplitFrame must fan a two-topic frame out to each topic; feeding
  // the whole array to one accumulator would append B's text onto A.
  const batched = JSON.stringify([
    {
      type: "message",
      topic_id: "conversation-turn-" + TURN_A,
      payload: { type: "conversation-turn-stream", payload: { type: "stream-item", conversation_id: CONV_A, turn_id: TURN_A, encoded_item: append("AAA") } },
    },
    {
      type: "message",
      topic_id: "conversation-turn-" + TURN_B,
      payload: { type: "conversation-turn-stream", payload: { type: "stream-item", conversation_id: CONV_B, turn_id: TURN_B, encoded_item: append("BBB") } },
    },
  ]);
  const splitParts = P.chatGPTWSSplitFrame(batched);
  eq("split: batched frame yields two parts", splitParts.length, 2);
  eq("split: part 0 topic A", splitParts[0].topicId, TURN_A);
  eq("split: part 1 topic B", splitParts[1].topicId, TURN_B);
  // Route the batched frame (preceded by the two subscribes + assistant adds so
  // each topic has an active target) through the tap; A and B must stay isolated.
  const batchEmits = runTap([
    subscribe(TURN_A),
    subscribe(TURN_B),
    frame(CONV_A, TURN_A, asst(CONV_A, "a1")),
    frame(CONV_B, TURN_B, asst(CONV_B, "b1")),
    batched, // ONE frame carrying an A append AND a B append
    done(CONV_A, TURN_A),
    done(CONV_B, TURN_B),
  ]);
  const byT = {};
  for (const e of batchEmits) byT[e.messageId] = e;
  eq("split: batched A text isolated", byT[TURN_A].text, "AAA");
  eq("split: batched B text isolated", byT[TURN_B].text, "BBB");
  eq("split: batched no B-in-A", byT[TURN_A].text.indexOf("BBB"), -1);
  eq("split: batched no A-in-B", byT[TURN_B].text.indexOf("AAA"), -1);

  // ---- R2-2: WS accumulator harvests BOTH dedup ids (handoff + working) ----
  // The topic/turn id (== messageId, == stream_handoff.turn_exchange_id / JWT
  // turn_topic_id) AND the working_turn_id (input_message/message metadata) are
  // both harvested — the transport dedups on EITHER (leg-1 SSE id unconfirmed).
  const WORKING = "bf89e158-cee5-442c-bd59-b9bb76853633";
  const ded = P.makeChatGPTWSAccumulator();
  ded.feed(frame(CONV_A, TURN_A, sse(null, { type: "stream_handoff", conversation_id: CONV_A, turn_exchange_id: TURN_A, options: [{ topic_id: "conversation-turn-" + TURN_A }] })));
  ded.feed(
    frame(
      CONV_A,
      TURN_A,
      sse(null, {
        type: "input_message",
        input_message: { author: { role: "user" }, content: { content_type: "text", parts: ["hi"] }, metadata: { working_turn_id: WORKING, resolved_model_slug: "gpt-5-6-thinking" } },
        conversation_id: CONV_A,
      })
    )
  );
  ded.feed(done(CONV_A, TURN_A));
  ded.finalize();
  eq("R2-2: handoffId harvested from stream_handoff (== topic turn_id)", ded.state.handoffId, TURN_A);
  eq("R2-2: workingTurnId harvested from input_message metadata", ded.state.workingTurnId, WORKING);
  eq("R2-2: messageId is the topic turn_id", ded.state.messageId, TURN_A);

  // handoffId can also come from the resume_conversation_token JWT.
  const b64u = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
  const jwt = b64u({ alg: "HS256", typ: "JWT" }) + "." + b64u({ turn_topic_id: "conversation-turn-" + TURN_A }) + ".sig";
  const dj = P.makeChatGPTWSAccumulator();
  dj.feed(frame(CONV_A, TURN_A, sse(null, { type: "resume_conversation_token", conversation_id: CONV_A, token: jwt })));
  dj.feed(done(CONV_A, TURN_A));
  dj.finalize();
  eq("R2-2: handoffId harvested from resume_conversation_token JWT", dj.state.handoffId, TURN_A);

  // ---- R2-3a: an un-`done` turn produces NO emit (silent eviction) ----
  const noDone = runTap([
    subscribe(TURN_A),
    frame(CONV_A, TURN_A, asst(CONV_A, "a1")),
    frame(CONV_A, TURN_A, append("partial, no done")),
    // no done frame — the tap never emits; content-main evicts silently on TTL.
  ]);
  eq("R2-3a: un-done turn never emits", noDone.length, 0);

  // ---- R2-3b: conversation-complete does NOT close a fresh EMPTY pending turn --
  const completeFrame = JSON.stringify([{ type: "message", topic_id: "conversations", payload: { type: "conversation-turn-complete", payload: { conversation_id: CONV_A } } }]);
  const emptyPending = runTap([
    subscribe(TURN_A),
    frame(CONV_A, TURN_A, asst(CONV_A, "a1")), // assistant target set, but NO answer text yet
    completeFrame, // same-conversation complete arrives while the turn is empty
  ]);
  eq("R2-3b: complete does not close an empty pending turn", emptyPending.length, 0);
  // But once the turn HAS text, a complete may close it (belt-and-braces).
  const withText = runTap([
    subscribe(TURN_A),
    frame(CONV_A, TURN_A, asst(CONV_A, "a1")),
    frame(CONV_A, TURN_A, append("has text")),
    completeFrame,
  ]);
  eq("R2-3b: complete closes a texted pending turn", withText.length, 1);
  eq("R2-3b: closed turn text intact", withText[0].text, "has text");

  // ---- R3: nested catchups routed PER-TOPIC (subscribe reply mixing A + B) ----
  // A subscribe reply's catchups[] must be split so each catchup routes by ITS
  // OWN topic — never all folded into the reply's topic (which would leak B's
  // text + workingTurnId/handoffId into A).
  const WORK_A = "aaaaaaaa-0000-4000-8000-000000000001";
  function catchupMsg(conv, turn, enc) {
    return {
      type: "message",
      topic_id: "conversation-turn-" + turn,
      payload: { type: "conversation-turn-stream", payload: { type: "stream-item", conversation_id: conv, turn_id: turn, encoded_item: enc } },
      offset: "o",
    };
  }
  function subscribeWithCatchups(subTurn, catchups) {
    return JSON.stringify([{ id: 9, type: "reply", reply: { type: "subscribe", topic_id: "conversation-turn-" + subTurn, recovered: true, catchups } }]);
  }
  // Reply subscribes topic A but its catchups mix A-data AND B-data.
  const mixedReply = subscribeWithCatchups(TURN_A, [
    catchupMsg(CONV_A, TURN_A, sse(null, { type: "stream_handoff", conversation_id: CONV_A, turn_exchange_id: TURN_A, options: [{ topic_id: "conversation-turn-" + TURN_A }] })),
    catchupMsg(CONV_A, TURN_A, asst(CONV_A, "a1")),
    catchupMsg(CONV_A, TURN_A, sse(null, { type: "input_message", input_message: { author: { role: "user" }, content: { content_type: "text", parts: ["prompt A"] }, metadata: { working_turn_id: WORK_A } }, conversation_id: CONV_A })),
    catchupMsg(CONV_A, TURN_A, append("A-answer")),
    catchupMsg(CONV_B, TURN_B, asst(CONV_B, "b1")),
    catchupMsg(CONV_B, TURN_B, append("B-answer")),
  ]);

  const mixedParts = P.chatGPTWSSplitFrame(mixedReply);
  ok("R3: first part is the turn-start for the reply topic", mixedParts[0].turnStart === true);
  eq("R3: turn-start topic is the reply topic (A)", mixedParts[0].topicId, TURN_A);
  // The turn-start part is catchup-FREE — feeding it carries no message data.
  const tsAcc = P.makeChatGPTWSAccumulator();
  tsAcc.feed(mixedParts[0].frame);
  tsAcc.finalize();
  eq("R3: turn-start part carries no answer text", tsAcc.state.text, "");
  eq("R3: turn-start part carries no prompt", tsAcc.state.prompt, "");
  const aParts = mixedParts.filter((p) => p.topicId === TURN_A && !p.turnStart);
  const bParts = mixedParts.filter((p) => p.topicId === TURN_B);
  ok("R3: has A-routed catchup parts", aParts.length >= 1);
  ok("R3: has B-routed catchup parts", bParts.length >= 1);
  ok("R3: NO catchup misrouted (every non-turn-start part is A or B)", mixedParts.every((p) => p.turnStart || p.topicId === TURN_A || p.topicId === TURN_B));

  // Route the mixed reply + both dones through the tap harness → full isolation.
  const mixedEmits = runTap([mixedReply, done(CONV_A, TURN_A), done(CONV_B, TURN_B)]);
  const m = {};
  for (const e of mixedEmits) m[e.messageId] = e;
  eq("R3: exactly two emits", mixedEmits.length, 2);
  eq("R3: A text isolated", m[TURN_A].text, "A-answer");
  eq("R3: B text isolated", m[TURN_B].text, "B-answer");
  eq("R3: no B text leaked into A", m[TURN_A].text.indexOf("B-answer"), -1);
  eq("R3: no A text leaked into B", m[TURN_B].text.indexOf("A-answer"), -1);
  // Dedup ids stay on their own topic — no cross-leak.
  eq("R3: A handoffId harvested from A catchup", m[TURN_A].handoffId, TURN_A);
  eq("R3: A workingTurnId harvested from A catchup", m[TURN_A].workingTurnId, WORK_A);
  eq("R3: B handoffId NOT leaked from A", m[TURN_B].handoffId, "");
  eq("R3: B workingTurnId NOT leaked from A", m[TURN_B].workingTurnId, "");

  // Happy-path: a subscribe reply whose catchups are ALL one topic → one turn.
  const singleTopicReply = subscribeWithCatchups(TURN_A, [
    catchupMsg(CONV_A, TURN_A, asst(CONV_A, "a1")),
    catchupMsg(CONV_A, TURN_A, append("single")),
  ]);
  const single = runTap([singleTopicReply, done(CONV_A, TURN_A)]);
  eq("R3: single-topic catchups → exactly one emit", single.length, 1);
  eq("R3: single-topic emit text intact", single[0].text, "single");
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

  // Request (2026-07-18 shape): prompt ALSO at top-level query_str, model at
  // params.model_preference, the per-turn id at params.frontend_uuid, the
  // thread/context id at params.frontend_context_uuid (NOT top-level). Structure
  // only — synthetic values.
  const req2 = P.parsePerplexityRequest(
    JSON.stringify({
      query_str: "What is 17 times 23?",
      params: {
        query_str: "What is 17 times 23?",
        dsl_query: "What is 17 times 23?",
        model_preference: "turbo",
        frontend_uuid: "fe-11111111",
        frontend_context_uuid: "ctx-22222222",
        mode: "copilot",
      },
    })
  );
  eq("perplexity request 2026 prompt", req2.prompt, "What is 17 times 23?");
  eq("perplexity request 2026 model", req2.model, "turbo");
  eq("perplexity request frontend_uuid (dedup key)", req2.frontendUuid, "fe-11111111");
  eq("perplexity request frontend_context_uuid → conversationId fallback", req2.conversationId, "ctx-22222222");

  // Request: prompt falls back to params.dsl_query when top-level query_str is
  // absent (some builds omit the top-level copy).
  const req3 = P.parsePerplexityRequest(
    JSON.stringify({ params: { dsl_query: "only in params", frontend_uuid: "fe-x" } })
  );
  eq("perplexity request prompt from params.dsl_query", req3.prompt, "only in params");

  // NEW 2026-07-17 answer shape: the visible answer moved from the FINAL-step
  // `data.text` JSON string to blocks[].workflow_block.steps[].items[]
  //   .payload.text_payload.text. ids: backend_uuid → conversationId,
  //   uuid → messageId, display_model → model. Structure only — synthetic.
  function blocksFrame(answer, opts) {
    return {
      backend_uuid: (opts && opts.backend_uuid) || "bk_new",
      uuid: (opts && opts.uuid) || "msg_new",
      display_model: (opts && opts.display_model) || "turbo",
      blocks: [
        {
          workflow_block: {
            steps: [
              { items: [{ payload: { text_payload: { text: answer } } }] },
            ],
          },
        },
      ],
    };
  }
  const ns = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([blocksFrame("Oslo is the capital of Norway.")])
  );
  eq("perplexity blocks-shape answer", ns.text, "Oslo is the capital of Norway.");
  eq("perplexity blocks-shape backend_uuid", ns.conversationId, "bk_new");
  eq("perplexity blocks-shape uuid", ns.messageId, "msg_new");
  eq("perplexity blocks-shape display_model", ns.model, "turbo");

  // Cumulative snapshots in the blocks shape — keep the LONGEST answer.
  const nsGrow = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([
      blocksFrame("Oslo"),
      blocksFrame("Oslo is the capital of Norway."),
    ])
  );
  eq("perplexity blocks-shape keeps longest", nsGrow.text, "Oslo is the capital of Norway.");

  // Multiple items in a step concatenate (streamed answer split across items).
  const multiItem = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([
      {
        backend_uuid: "bk_m",
        uuid: "u_m",
        blocks: [
          {
            workflow_block: {
              steps: [
                {
                  items: [
                    { payload: { text_payload: { text: "The answer " } } },
                    { payload: { text_payload: { text: "is 391." } } },
                  ],
                },
              ],
            },
          },
        ],
      },
    ])
  );
  eq("perplexity blocks multi-item concatenation", multiItem.text, "The answer is 391.");

  // Malformed blocks (missing text_payload / wrong nesting) must NOT throw and
  // fall through to the FINAL-step / plain-answer fallbacks.
  const malformedBlocks = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([
      { backend_uuid: "bk_z", blocks: [{ workflow_block: { steps: [{ items: [{ payload: {} }] }] } }], answer: "fallback wins" },
    ])
  );
  eq("perplexity malformed blocks → plain-answer fallback", malformedBlocks.text, "fallback wins");

  // REAL 2026-07-18 shape (live capture): the answer lives in a
  // `markdown_block.answer` (+ `.chunks[]`), and a SINGLE snapshot carries the
  // SAME answer in TWO blocks — intended_usage "ask_text_0_markdown" AND the
  // canonical "ask_text". Naive concatenation double-counts ("100" + "100" →
  // "100100"); the parser must PREFER the "ask_text" block. Structure mirrors
  // the live wire (plan_block / markdown_block / answer_tabs_block /
  // pending_followups_block); values synthetic.
  const realMarkdown = {
    backend_uuid: "6affa4cb-real",
    uuid: "215233f4-real",
    display_model: "turbo",
    text_completed: true,
    blocks: [
      { intended_usage: "plan", plan_block: { progress: "DONE", goals: [], final: true } },
      { intended_usage: "ask_text_0_markdown", markdown_block: { progress: "DONE", chunks: ["100"], answer: "100" } },
      { intended_usage: "ask_text", markdown_block: { progress: "DONE", chunks: ["100"], answer: "100" } },
      { intended_usage: "answer_tabs", answer_tabs_block: { modes: [], reformulated_queries: [] } },
      { intended_usage: "pending_followups", pending_followups_block: { followups: [] } },
    ],
  };
  const rm = runSSE(P.makePerplexityAccumulator(), dataLines([realMarkdown]));
  eq("perplexity markdown_block answer (no double-count)", rm.text, "100");
  eq("perplexity markdown_block backend_uuid", rm.conversationId, "6affa4cb-real");
  eq("perplexity markdown_block uuid", rm.messageId, "215233f4-real");
  eq("perplexity markdown_block display_model", rm.model, "turbo");

  // markdown_block chunks[] concatenate WITHIN one block (streamed pieces of ONE
  // answer) — but distinct blocks never concatenate.
  const chunked = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([
      {
        backend_uuid: "bk_c",
        blocks: [
          { intended_usage: "ask_text", markdown_block: { chunks: ["The answer ", "is ", "391."] } },
        ],
      },
    ])
  );
  eq("perplexity markdown_block chunks concatenated within block", chunked.text, "The answer is 391.");

  // When no "ask_text" block is present, fall back to the LONGEST block (best
  // effort), never a concatenation of unrelated blocks.
  const noCanonical = runSSE(
    P.makePerplexityAccumulator(),
    dataLines([
      {
        backend_uuid: "bk_l",
        blocks: [
          { intended_usage: "plan", markdown_block: { answer: "short" } },
          { intended_usage: "ask_text_0_markdown", markdown_block: { answer: "the longer real answer" } },
        ],
      },
    ])
  );
  eq("perplexity no-ask_text falls back to longest block", noCanonical.text, "the longer real answer");

  // --- request: last_backend_uuid (thread chain link) --------------------
  // LIVE-CONFIRMED 2026-07-18 (multi-turn recon): a FOLLOW-UP ask carries
  // params.last_backend_uuid = the PREVIOUS turn's backend_uuid; the FIRST ask
  // from home does NOT.
  const reqFollow = P.parsePerplexityRequest(
    JSON.stringify({
      query_str: "and squared?",
      params: {
        query_str: "and squared?",
        model_preference: "turbo",
        frontend_uuid: "fe-follow",
        last_backend_uuid: "bk-prev-turn",
        query_source: "followup",
      },
    })
  );
  eq("perplexity follow-up last_backend_uuid harvested", reqFollow.lastBackendUuid, "bk-prev-turn");
  eq("perplexity follow-up frontend_uuid", reqFollow.frontendUuid, "fe-follow");
  const reqHome = P.parsePerplexityRequest(
    JSON.stringify({ query_str: "first ask", params: { frontend_uuid: "fe-home", query_source: "home" } })
  );
  eq("perplexity home ask has no last_backend_uuid", reqHome.lastBackendUuid, "");

  // --- perplexityThreadIdFromPathname (pure) -----------------------------
  eq(
    "perplexity thread id from /search/<uuid>",
    P.perplexityThreadIdFromPathname("/search/f54b32bb-thread"),
    "f54b32bb-thread"
  );
  eq(
    "perplexity thread id ignores trailing path/query/hash",
    P.perplexityThreadIdFromPathname("/search/f54b32bb-thread/foo?x=1#y"),
    "f54b32bb-thread"
  );
  eq("perplexity thread id: non-search path → ''", P.perplexityThreadIdFromPathname("/"), "");
  eq("perplexity thread id: settings path → ''", P.perplexityThreadIdFromPathname("/settings/account"), "");
  eq("perplexity thread id: non-string → ''", P.perplexityThreadIdFromPathname(null), "");

  // --- (a) 3-ask multi-turn thread via the CHAIN-MAP path ----------------
  // The URL is UNAVAILABLE (pathname ""), forcing the last_backend_uuid chain.
  // Ask 1 (home): no last_backend_uuid, own backend B1 → thread B1 (stream).
  // Ask 2/3 (followups): last_backend_uuid = the prior turn's own backend → the
  // SAME thread B1. All three turns must resolve to ONE conversation id.
  (function chainMapThread() {
    const B1 = "f54b32bb-ask1";
    const B2 = "aa111111-ask2";
    const B3 = "bb222222-ask3";
    const r = P.makePerplexityThreadResolver();
    const t1 = r.resolve({ pathname: "", ownBackendUuid: B1, frontendUuid: "fe1" });
    eq("perplexity chain ask1 → own backend (thread seed)", t1.conversationId, B1);
    eq("perplexity chain ask1 id_source stream", t1.idSource, "stream");
    const t2 = r.resolve({ pathname: "", lastBackendUuid: B1, ownBackendUuid: B2, frontendUuid: "fe2" });
    eq("perplexity chain ask2 resolves to thread B1", t2.conversationId, B1);
    eq("perplexity chain ask2 id_source chain", t2.idSource, "chain");
    const t3 = r.resolve({ pathname: "", lastBackendUuid: B2, ownBackendUuid: B3, frontendUuid: "fe3" });
    eq("perplexity chain ask3 resolves to thread B1", t3.conversationId, B1);
    ok(
      "perplexity chain: ONE conversation id across all 3 asks",
      t1.conversationId === t2.conversationId && t2.conversationId === t3.conversationId
    );
  })();

  // --- (b) URL priority wins over chain + own backend --------------------
  (function urlPriority() {
    const THREAD = "f54b32bb-thread";
    const r = P.makePerplexityThreadResolver();
    // The URL is present and DIFFERS from both the own backend AND a stale chain
    // link — the URL must win regardless.
    const t = r.resolve({
      pathname: "/search/" + THREAD,
      lastBackendUuid: "some-old-backend",
      ownBackendUuid: "this-turn-backend",
      frontendUuid: "fe-x",
    });
    eq("perplexity URL priority: thread id from URL", t.conversationId, THREAD);
    eq("perplexity URL priority: id_source url", t.idSource, "url");
    // And the URL path records own→thread, so a LATER chain turn (no URL) that
    // chains from this turn's own backend still lands on the URL thread.
    const t2 = r.resolve({ pathname: "", lastBackendUuid: "this-turn-backend", ownBackendUuid: "next", frontendUuid: "fe-y" });
    eq("perplexity URL priority: subsequent chain inherits URL thread", t2.conversationId, THREAD);
  })();

  // --- (c) reopen: dead map, last_backend_uuid present → resolves to L ----
  // A reopened thread's in-memory map is empty. The first follow-up chains from
  // an UNMAPPED last_backend_uuid L, so it resolves to L itself and every later
  // turn stays on L (stable within the reopened page, even if L ≠ the true
  // first-ask thread id — the /search URL is the real fix; this is the fallback).
  (function reopenChain() {
    const L = "cc333333-last-before-reopen";
    const r = P.makePerplexityThreadResolver(); // fresh page → empty map
    const t1 = r.resolve({ pathname: "", lastBackendUuid: L, ownBackendUuid: "dd444444-reopen1", frontendUuid: "fe-r1" });
    eq("perplexity reopen: unmapped L resolves to L itself", t1.conversationId, L);
    eq("perplexity reopen id_source chain", t1.idSource, "chain");
    const t2 = r.resolve({ pathname: "", lastBackendUuid: "dd444444-reopen1", ownBackendUuid: "ee555555-reopen2", frontendUuid: "fe-r2" });
    eq("perplexity reopen: subsequent turn stays on L", t2.conversationId, L);
  })();

  // --- request-id fallback (no URL, no chain, no backend harvested) ------
  (function requestFallback() {
    const r = P.makePerplexityThreadResolver();
    const t = r.resolve({ pathname: "", ownBackendUuid: "", requestConversationId: "ctx-fallback", frontendUuid: "fe-z" });
    eq("perplexity fallback: request conversation id", t.conversationId, "ctx-fallback");
    eq("perplexity fallback: id_source request", t.idSource, "request");
    const t2 = P.makePerplexityThreadResolver().resolve({ pathname: "", frontendUuid: "fe-only" });
    eq("perplexity fallback: frontend_uuid last-ditch", t2.conversationId, "fe-only");
    const t3 = P.makePerplexityThreadResolver().resolve({ pathname: "" });
    eq("perplexity fallback: nothing → '' / none", t3.conversationId, "");
    eq("perplexity fallback: id_source none", t3.idSource, "none");
  })();

  // --- (r3-2) request-time pathname snapshot wins the completion-time race ---
  (function startPathnameRace() {
    const r = P.makePerplexityThreadResolver();
    // A slow ask BEGUN in /search/A completes AFTER an in-SPA nav to /search/B:
    // the START snapshot wins, so the turn is attributed to A (not B).
    const t = r.resolve({
      startPathname: "/search/A-thread",
      pathname: "/search/B-thread", // where the SPA navigated mid-stream
      ownBackendUuid: "own-b",
      frontendUuid: "fe-race",
    });
    eq("perplexity r3-2: start-thread wins the nav race", t.conversationId, "A-thread");
    eq("perplexity r3-2: id_source url", t.idSource, "url");
    // A request begun OUTSIDE a thread (home) whose URL flips mid-stream to
    // /search/<own-backend_uuid> (the LEGITIMATE first-ask flip) DOES adopt the
    // completion-time pathname — it AGREES with own, so it is trustworthy.
    const t2 = P.makePerplexityThreadResolver().resolve({
      startPathname: "/", // home — no thread at request time
      pathname: "/search/own-c", // flipped to /search/<own> during the first ask
      ownBackendUuid: "own-c",
      frontendUuid: "fe-first",
    });
    eq("perplexity r3-2: first-ask adopts AGREEING completion pathname", t2.conversationId, "own-c");
    eq("perplexity r3-2: agreeing first-ask id_source url", t2.idSource, "url");
  })();

  // --- (re-review MED) home ask + mid-stream navigation to ANOTHER thread ---
  // A FIRST ask begins on `/` (no lastBackendUuid), harvests its own backend
  // uuid, then the user navigates to a DIFFERENT existing thread /search/OTHER
  // before the ask completes. The completion-time pathname now points at the
  // wrong thread; because it does NOT agree with own, the resolver must PREFER
  // own (id_source "stream"), never misattribute the turn to /search/OTHER.
  (function homeAskMidStreamNav() {
    const r = P.makePerplexityThreadResolver();
    const t = r.resolve({
      startPathname: "/", // home — first ask, no thread at request time
      pathname: "/search/OTHER-thread", // user navigated away mid-stream
      ownBackendUuid: "own-first-ask",
      frontendUuid: "fe-nav",
    });
    eq("perplexity home-ask mid-nav: prefers own backend, not the wrong URL", t.conversationId, "own-first-ask");
    eq("perplexity home-ask mid-nav: id_source stream (not url)", t.idSource, "stream");
    ok(
      "perplexity home-ask mid-nav: never attributes to the navigated-to thread",
      t.conversationId !== "OTHER-thread",
    );
    // The NEXT follow-up (chained from this turn's own backend) then lands on
    // the same thread key — the chain map recorded own→own.
    const t2 = r.resolve({ pathname: "", lastBackendUuid: "own-first-ask", ownBackendUuid: "own-followup", frontendUuid: "fe-nav2" });
    eq("perplexity home-ask mid-nav: follow-up chains to the first-ask thread", t2.conversationId, "own-first-ask");
  })();
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

  // REGRESSION (adversarial review r2-2): harvest the DISPLAYED candidate (index
  // 0), NOT the longest candidate. A frame with a SHORT shown candidate 0 and a
  // LONGER unselected alternative at index 1 must record candidate 0's text.
  const multiCand = envelope([
    wrbEntry(
      partJson("cid_m", "rid_m", [
        candidate("rc_shown", "Shown answer.", true),
        candidate("rc_alt", "A MUCH longer unselected alternative answer nobody sees.", true),
      ]),
    ),
  ]);
  const accMulti = P.makeGeminiAccumulator();
  accMulti.feed(multiCand);
  accMulti.finalize();
  eq("gemini harvests displayed candidate 0 (not longest)", accMulti.state.text, "Shown answer.");

  // REGRESSION (adversarial review r2-3): a trailing control record whose pj[1]
  // is a two-string array (e.g. ["status","complete"]) must NOT overwrite a real
  // answer's c_*/r_* ids. Canonical ids latch; the later non-canonical record is
  // rejected.
  const answerFr = [
    ["wrb.fr", null, JSON.stringify(
      (function () { const c = ["rc_a", ["The answer."]]; c[8] = [1]; return [null, ["c_real123", "r_real456"], null, null, [c]]; })(),
    )],
  ];
  const controlFr = [["wrb.fr", null, JSON.stringify([null, ["status", "complete"], { "18": "x" }])]];
  const accCtl = P.makeGeminiAccumulator();
  // Feed a hand-built envelope with WRONG length prefixes (line-based parser).
  let ctlEnv = ")]}'\n\n";
  for (const fr of [answerFr, controlFr]) {
    const s = JSON.stringify(fr);
    ctlEnv += (s.length + 3) + "\n" + s + "\n";
  }
  accCtl.feed(ctlEnv);
  accCtl.finalize();
  eq("gemini control record does NOT overwrite conversation id", accCtl.state.conversationId, "c_real123");
  eq("gemini control record does NOT overwrite response id", accCtl.state.messageId, "r_real456");
  eq("gemini answer survives trailing control record", accCtl.state.text, "The answer.");

  // REGRESSION (LIVE 2026-07-18): the real StreamGenerate wire declares a frame
  // LENGTH that DISAGREES with the JS code-unit length of the payload (177 vs
  // 175), so the old length-slice parser cut every frame mid-string and
  // harvested NOTHING (captures:0 / empties++). The parser is now line-based and
  // ignores the (unreliable) length prefix entirely. This fixture uses the REAL
  // shape — inner[1][0]=c_ cid, inner[1][1]=r_ rid, inner[4][0][0]=rc_ candidate
  // id, answer at inner[4][0][1][0] — with DELIBERATELY WRONG length prefixes to
  // prove length is ignored. Synthetic values.
  function realEnvelope(frameArrs) {
    let out = ")]}'\n\n";
    for (const fr of frameArrs) {
      const s = JSON.stringify(fr);
      // WRONG length on purpose (real wire is off by a couple; +7 here).
      out += (s.length + 7) + "\n" + s + "\n";
    }
    return out;
  }
  function realInner(cid, rid, rcid, text) {
    // [null, [cid, rid], null, null, [[rcid, [text], ... [1]]]]
    const cand = [rcid, [text]];
    cand[8] = [1];
    return [null, [cid, rid], null, null, [cand]];
  }
  function realWrb(inner) {
    return ["wrb.fr", null, JSON.stringify(inner), null, null, null, "generic"];
  }
  const idFrame = [["wrb.fr", null, JSON.stringify([null, ["c_abc123def456", "r_deadbeef00"], { "18": "r_deadbeef00" }])]];
  const ansFrame1 = [realWrb(realInner("c_abc123def456", "r_deadbeef00", "rc_cand01", "17 times 23 is"))];
  const ansFrame2 = [realWrb(realInner("c_abc123def456", "r_deadbeef00", "rc_cand01", "17 times 23 is 391."))];
  const realAcc = P.makeGeminiAccumulator();
  const env = realEnvelope([idFrame, ansFrame1, ansFrame2]);
  // Feed split across a mid-frame chunk boundary — the accumulator buffers whole.
  const cut = Math.floor(env.length * 0.4);
  realAcc.feed(env.slice(0, cut));
  realAcc.feed(env.slice(cut));
  realAcc.finalize();
  eq("gemini real-shape answer (wrong length ignored)", realAcc.state.text, "17 times 23 is 391.");
  eq("gemini real-shape c_ conversation id", realAcc.state.conversationId, "c_abc123def456");
  eq("gemini real-shape r_ response id", realAcc.state.messageId, "r_deadbeef00");

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

// ============ ChatGPT robust conversation_id harvest ====================
(function chatgptConvIdHarvest() {
  // (a) id ONLY in message.metadata.conversation_id (a final assistant
  // snapshot / metadata frame whose top-level omits it).
  const metaOnly = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      {
        p: "",
        o: "add",
        v: {
          message: {
            id: "m_meta",
            author: { role: "assistant" },
            content: { parts: ["hi"] },
            metadata: { model_slug: "gpt-5-6", conversation_id: "cid_meta" },
          },
        },
      },
      "[DONE]",
    ])
  );
  eq("convId from message.metadata.conversation_id", metaOnly.conversationId, "cid_meta");

  // (b) id delivered via a delta-encoded patch sub-op path /conversation_id.
  const patchPath = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      { p: "/message/content/parts/0", o: "append", v: "answer" },
      { o: "patch", p: "", v: [{ p: "/conversation_id", o: "replace", v: "cid_patch" }] },
      "[DONE]",
    ])
  );
  eq("convId from /conversation_id patch path", patchPath.conversationId, "cid_patch");

  // (c) id in a final NON-message object frame (conversation_detail_metadata).
  const finalFrame = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      { p: "/message/content/parts/0", o: "append", v: "answer" },
      { p: "", o: "add", v: { conversation_id: "cid_final", type: "conversation_detail_metadata" } },
      "[DONE]",
    ])
  );
  eq("convId from final metadata frame", finalFrame.conversationId, "cid_final");

  // (d) first non-empty wins — a later empty must NOT clobber a real id.
  const noClobber = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      { conversation_id: "cid_first" },
      { conversation_id: "" },
      { p: "/message/content/parts/0", o: "append", v: "x" },
      "[DONE]",
    ])
  );
  eq("convId first-non-empty wins", noClobber.conversationId, "cid_first");

  // (e) new-chat leg 1: id absent EVERYWHERE + handoff-only → id-less but
  // STILL handoff-pending (so the transport buffers it under a synthetic key).
  const idless = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([{ type: "stream_handoff" }, "[DONE]"])
  );
  eq("id-less leg1 conversationId empty", idless.conversationId, "");
  ok("id-less leg1 still handoff-pending", P.chatGPTIsHandoffPending(idless) === true);
})();

// ============ dead URL harvester removed ================================
(function deadHarvesterRemoved() {
  // conversationIdFromChatGPTURL was UNREACHABLE: isCaptureURL never admits a
  // `/conversation/<uuid>` path (the turn POST is the bare base + /resume), and
  // recon does not list that path as a turn carrier. It was removed — the id
  // comes from the request body + SSE frames instead. Assert it's gone.
  ok("conversationIdFromChatGPTURL removed", typeof P.conversationIdFromChatGPTURL === "undefined");

  // The /resume request BODY carries {conversation_id, offset} — harvested by
  // the request parser (the id carrier when the answer streams on leg 2). This
  // is the real id path that replaced the dead URL harvester.
  const resumeReq = P.parseChatGPTRequest(JSON.stringify({ conversation_id: "cid_resume", offset: 0 }));
  eq("resume body conversation_id", resumeReq.conversationId, "cid_resume");
})();

// ============ two-leg correlator harvest (MED-3) ========================
(function correlatorHarvest() {
  // base64url-encode a synthetic conduit JWT so turnTopicFromJWT can decode it.
  // Real structure (recon chatgpt-wire-findings.md), synthetic values.
  function makeJWT(claims) {
    const b64 = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
    return b64({ alg: "HS256", typ: "JWT" }) + "." + b64(claims) + ".sig_ignored";
  }

  // normalizeTurnId: turn_exchange_id is the raw <tx>; topic_id /
  // turn_topic_id are "conversation-turn-<tx>" — all three normalize to <tx>.
  eq("normalizeTurnId strips prefix", P.normalizeTurnId("conversation-turn-tx_9"), "tx_9");
  eq("normalizeTurnId passes raw through", P.normalizeTurnId("tx_9"), "tx_9");
  eq("normalizeTurnId empty", P.normalizeTurnId(""), "");
  eq("normalizeTurnId non-string", P.normalizeTurnId(null), "");

  // JWT decode: turn_topic_id claim → normalized <tx>.
  const jwt = makeJWT({ conduit_uuid: "cu", turn_topic_id: "conversation-turn-tx_jwt", exp: 1 });
  eq("turnTopicFromJWT decodes claim", P.turnTopicFromJWT(jwt), "tx_jwt");
  eq("turnTopicFromJWT garbage → empty", P.turnTopicFromJWT("not.a.jwt"), "");
  eq("turnTopicFromJWT non-string → empty", P.turnTopicFromJWT(null), "");
  ok("decodeJWTPayload reads claims", P.decodeJWTPayload(jwt).conduit_uuid === "cu");
  eq("decodeJWTPayload garbage → null", P.decodeJWTPayload("x"), null);
  // LOW-1: require EXACTLY three non-empty dot-separated segments. A 2-segment
  // (`header.claims`) or 4-segment (`a.b.c.d`) token is malformed → null, even
  // though the old one-dot check would happily have decoded segment 2.
  const b64 = (o) => Buffer.from(JSON.stringify(o)).toString("base64url");
  const claimsSeg = b64({ turn_topic_id: "conversation-turn-tx_x" });
  eq("decodeJWTPayload rejects 2-segment token", P.decodeJWTPayload("hdr." + claimsSeg), null);
  eq(
    "decodeJWTPayload rejects 4-segment token",
    P.decodeJWTPayload("hdr." + claimsSeg + ".sig.extra"),
    null
  );
  eq("decodeJWTPayload rejects empty middle segment", P.decodeJWTPayload("hdr..sig"), null);
  eq("decodeJWTPayload rejects non-string", P.decodeJWTPayload(null), null);
  ok(
    "decodeJWTPayload accepts valid 3-segment token",
    P.decodeJWTPayload("hdr." + claimsSeg + ".sig").turn_topic_id === "conversation-turn-tx_x"
  );
  // The two-leg correlator's public wrapper stays fail-soft on the rejected
  // shapes (2-/4-segment → "").
  eq("turnTopicFromJWT rejects 2-segment token", P.turnTopicFromJWT("hdr." + claimsSeg), "");
  eq(
    "turnTopicFromJWT rejects 4-segment token",
    P.turnTopicFromJWT("hdr." + claimsSeg + ".sig.extra"),
    ""
  );

  // handoffIdFromFrame precedence: turn_exchange_id first, else options topic.
  eq(
    "handoffIdFromFrame prefers turn_exchange_id",
    P.handoffIdFromFrame({ turn_exchange_id: "tx_a", options: [{ topic_id: "conversation-turn-tx_b" }] }),
    "tx_a"
  );
  eq(
    "handoffIdFromFrame falls back to options topic_id",
    P.handoffIdFromFrame({ options: [{ topic_id: "conversation-turn-tx_c" }] }),
    "tx_c"
  );
  eq("handoffIdFromFrame none → empty", P.handoffIdFromFrame({}), "");

  // Accumulator harvests handoffId from the leg-1 stream_handoff frame.
  const leg1 = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      {
        type: "stream_handoff",
        turn_exchange_id: "tx_1",
        options: [{ type: "resume_sse_endpoint", topic_id: "conversation-turn-tx_1" }],
      },
      "[DONE]",
    ])
  );
  eq("accumulator harvests handoffId from stream_handoff", leg1.handoffId, "tx_1");
  ok("leg1 still handoff-pending", P.chatGPTIsHandoffPending(leg1) === true);

  // Accumulator harvests handoffId from the resume_conversation_token JWT
  // (present on BOTH legs — the common carrier when a frame lacks the raw tx).
  const legJwt = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      { type: "resume_conversation_token", kind: "topic", token: makeJWT({ turn_topic_id: "conversation-turn-tx_2" }) },
      { type: "stream_handoff" },
      "[DONE]",
    ])
  );
  eq("accumulator harvests handoffId from JWT", legJwt.handoffId, "tx_2");

  // First non-empty wins (a later frame doesn't clobber the harvested id).
  const firstWins = runSSE(
    P.makeChatGPTAccumulator(),
    dataLines([
      { type: "stream_handoff", turn_exchange_id: "tx_first" },
      { type: "stream_handoff", turn_exchange_id: "tx_second" },
      "[DONE]",
    ])
  );
  eq("handoffId first-non-empty wins", firstWins.handoffId, "tx_first");
})();

// ============ selectHandoffPending (overlap-safe pairing, MED-3) =========
(function pairing() {
  // Model the transport's pending list: {key, correlatorId, conversationId,
  // synthetic}. selectHandoffPending returns { entry, via }.

  // (1) correlator pairing — TWO id-less handoffs overlap in one tab; an
  // out-of-order resume A must pair to synthetic A by its correlator id, NOT
  // the newest synthetic (which the old takeNewestSyntheticPending would grab).
  const A = { key: "syn_A", correlatorId: "tx_A", conversationId: "", synthetic: true };
  const B = { key: "syn_B", correlatorId: "tx_B", conversationId: "", synthetic: true };
  // B was buffered LAST (newest). Resume A carries tx_A but its own convId.
  const selA = P.selectHandoffPending([A, B], { correlatorId: "tx_A", conversationId: "cid_A" });
  eq("overlap: resume A pairs to leg1 A (not newest B)", selA.entry && selA.entry.key, "syn_A");
  eq("overlap: via correlator", selA.via, "correlator");
  const selB = P.selectHandoffPending([A, B], { correlatorId: "tx_B", conversationId: "cid_B" });
  eq("overlap: resume B pairs to leg1 B", selB.entry && selB.entry.key, "syn_B");

  // (2) conversation-id pairing — a non-synthetic leg-1 keyed by convId.
  const R = { key: "cid_R", correlatorId: "", conversationId: "cid_R", synthetic: false };
  const selR = P.selectHandoffPending([R], { correlatorId: "", conversationId: "cid_R" });
  eq("convId pairing", selR.entry && selR.entry.key, "cid_R");
  eq("convId pairing via", selR.via, "conversation");

  // (3) resume with a REAL convId that matches nothing must NOT adopt a
  // synthetic entry (MED-3c) — it emits as-is (no match).
  const selNoRaid = P.selectHandoffPending([A], { correlatorId: "", conversationId: "cid_unmatched" });
  eq("real-convId resume never raids synthetic", selNoRaid.entry, null);
  eq("real-convId unmatched via empty", selNoRaid.via, "");

  // (4) id-less resume + exactly ONE synthetic pending → adopt it.
  const selSole = P.selectHandoffPending([A], { correlatorId: "", conversationId: "" });
  eq("id-less resume adopts sole synthetic", selSole.entry && selSole.entry.key, "syn_A");
  eq("sole synthetic via", selSole.via, "synthetic");

  // (5) id-less resume + TWO synthetic pending → ambiguous → NO match (the
  // buffered legs emit alone on their flush timers).
  const selAmbig = P.selectHandoffPending([A, B], { correlatorId: "", conversationId: "" });
  eq("ambiguous id-less resume → no synthetic guess", selAmbig.entry, null);
  eq("ambiguous via empty", selAmbig.via, "");

  // (6) correlator wins even when a (different) synthetic is also present —
  // precise pairing takes precedence over the sole-synthetic fallback.
  const selPrec = P.selectHandoffPending([A, B], { correlatorId: "tx_B", conversationId: "" });
  eq("correlator precedence over synthetic fallback", selPrec.entry && selPrec.entry.key, "syn_B");
  eq("correlator precedence via", selPrec.via, "correlator");

  // (7) empty list → no match, never throws.
  eq("empty pending → no match", P.selectHandoffPending([], { correlatorId: "x", conversationId: "y" }).entry, null);
})();

// ============ resolveIdSource (id provenance tag) =======================
(function idSource() {
  eq("idSource none", P.resolveIdSource("", "", undefined), "none");
  eq("idSource request", P.resolveIdSource("", "cid_req", undefined), "request");
  eq("idSource stream", P.resolveIdSource("cid_stream", "cid_req", undefined), "stream");
  eq("idSource override wins (resume)", P.resolveIdSource("cid_stream", "cid_req", "resume"), "resume");
  // id_source values are the exact set content-isolated.applyGranularity now
  // plumbs through to the wire (metadata, not content — always rides). Any Go
  // struct field to STORE it can rely on this closed value set.
  ok(
    "idSource values are the plumbed set",
    ["none", "request", "stream", "resume"].indexOf(P.resolveIdSource("a", "b", "resume")) !== -1
  );
})();

// ============ deriveHealthBeacon (beacon honesty) ======================
(function healthBeacon() {
  eq("beacon ok status", P.deriveHealthBeacon({ captures: 3, empties: 0, id_missing: 0 }).status, "ok");
  eq(
    "beacon degraded (parser stale)",
    P.deriveHealthBeacon({ captures: 0, empties: 4, id_missing: 0 }).status,
    "degraded"
  );
  const idDeg = P.deriveHealthBeacon({ captures: 5, empties: 0, id_missing: 5 });
  eq("beacon ok-degraded-id when all captures lack id", idDeg.status, "ok-degraded-id");
  ok("beacon ok-degraded-id reason mentions conversation_id", idDeg.reason.indexOf("conversation_id") !== -1);
  eq(
    "beacon ok when SOME captures carry id",
    P.deriveHealthBeacon({ captures: 5, empties: 0, id_missing: 2 }).status,
    "ok"
  );
  const idle = P.deriveHealthBeacon({ site: "chatgpt-web", captures: 0, empties: 0, status: "idle" });
  eq("beacon honors explicit idle status", idle.status, "idle");
  ok("beacon idle reason mentions page load", idle.reason.indexOf("page load") !== -1);

  // MED-4 priority ranking: the idle heartbeat is "low", every real status is
  // "normal". CONTRACT for the Go reader: a "low" beacon must not overwrite a
  // recent "normal" record for the same site (health is keyed by site, and JS
  // can't see other tabs, so an idle background tab must not clobber a
  // capturing tab). Additive/inert until Go honors it.
  eq("beacon idle priority is low", idle.priority, "low");
  eq("beacon ok priority is normal", P.deriveHealthBeacon({ captures: 3, empties: 0, id_missing: 0 }).priority, "normal");
  eq("beacon degraded priority is normal", P.deriveHealthBeacon({ captures: 0, empties: 4 }).priority, "normal");
  eq(
    "beacon ok-degraded-id priority is normal",
    P.deriveHealthBeacon({ captures: 5, empties: 0, id_missing: 5 }).priority,
    "normal"
  );
})();

// ====== hasResponseContent (mid-stream-error emit gate) =================
// The strict "genuine response content" predicate the fetch/SSE stream-reader
// error branch uses to decide partial-capture-emit vs failed-request-drop.
// Distinct from emitTurn's loose hasSomething (conversationId || text ||
// prompt): a prompt / conversation id alone is NOT response content, so a
// stream that broke before any assistant text must NOT emit and must NOT
// disarm the idle canary.
(function hasResponseContent() {
  ok("real assistant text is content", P.hasResponseContent({ text: "Hello there." }) === true);
  ok("text alongside id is content", P.hasResponseContent({ text: "A", conversationId: "c1" }) === true);
  ok("empty text is NOT content", P.hasResponseContent({ text: "" }) === false);
  ok("missing text is NOT content", P.hasResponseContent({ conversationId: "c1" }) === false);
  // conversation id / prompt-only shape (a failed request that never answered).
  ok("conv-id only is NOT content", P.hasResponseContent({ conversationId: "c1", messageId: "m1" }) === false);
  ok("null state is NOT content", P.hasResponseContent(null) === false);
  ok("undefined state is NOT content", P.hasResponseContent(undefined) === false);
  // non-string text (defensive) is NOT content.
  ok("non-string text is NOT content", P.hasResponseContent({ text: 123 }) === false);
  ok("whitespace text counts (length>0, harvested)", P.hasResponseContent({ text: " " }) === true);
})();

// ============================ Copilot ==================================
// LIVE-CONFIRMED 2026-07-18 (copilot recon): copilot.microsoft.com is a pure
// WebSocket transport. Frames below are the REAL wire shapes (ids anonymized).
// feedCopilot folds an array of already-JSON.parsed server frames through the
// pure accumulator, mirroring content-main's per-frame message handler.
(function copilot() {
  function feedCopilot(acc, frames) {
    for (const f of frames) acc.feed(f);
    return acc.state;
  }

  const CONV = "9SBqc2jHN6ZEcQfJF7iZX";
  const ASSISTANT_MSG = "JtnY1HvWcaqdJ6KZvCaCZ";
  const USER_MSG = "fsb9dn8eQwKhSsXopxwbR";

  // --- send frame: prompt is a content-PARTS ARRAY (the ingest-blocking bug) --
  const sendFrame = {
    event: "send",
    conversationId: CONV,
    content: [{ type: "text", text: "What is the capital of Japan?" }],
    mode: "smart",
    context: {},
  };
  const parsed = P.parseCopilotSendFrame(sendFrame);
  // prompt_text MUST be a joined STRING — the old code passed the raw array
  // through, which the daemon rejects (`cannot unmarshal array into ...string`).
  eq("copilot prompt joined to STRING", parsed.prompt, "What is the capital of Japan?");
  ok("copilot prompt is a string, not an array", typeof parsed.prompt === "string");
  ok("copilot prompt is NOT the raw array", Array.isArray(parsed.prompt) === false);
  eq("copilot model harvested from send `mode` verbatim", parsed.model, "smart");
  eq("copilot conversationId from send frame", parsed.conversationId, CONV);

  // --- multi-part content: join ONLY text parts, ignore non-text (e.g. image) --
  const multiPart = P.parseCopilotSendFrame({
    event: "send",
    conversationId: CONV,
    content: [
      { type: "text", text: "Describe this" },
      { type: "image", url: "blob:ignored" },
      { type: "text", text: "in one line." },
    ],
    mode: "smart",
  });
  eq("copilot joins only text parts (image ignored)", multiPart.prompt, "Describe this\nin one line.");
  ok("copilot multi-part prompt is a string", typeof multiPart.prompt === "string");

  // copilotJoinContent directly: array → joined string; plain string passes
  // through; non-array/non-string → "".
  eq("copilotJoinContent array", P.copilotJoinContent([{ type: "text", text: "a" }, { type: "text", text: "b" }]), "a\nb");
  eq("copilotJoinContent plain string passthrough", P.copilotJoinContent("already a string"), "already a string");
  eq("copilotJoinContent undefined → empty", P.copilotJoinContent(undefined), "");

  // --- array user-echo → joined STRING prompt_text (the ingest-blocking bug) --
  // The consumer-Copilot completion frame echoes the user turn as an array of
  // adaptive-card / message-part segments. When that array reaches prompt_text
  // as-is the daemon hard-fails the whole turn (`cannot unmarshal array into Go
  // struct field CapturedTurn.prompt_text of type string`). Both the send-frame
  // parser AND the emission chokepoint (coerceContentText) must collapse it to
  // one joined string. Reconstructed from the fixture send-frame shape above.
  const echoArray = [
    { type: "text", text: "What is the" },
    { type: "text", text: "capital of Japan?" },
  ];
  const echoParsed = P.parseCopilotSendFrame({
    event: "send",
    conversationId: CONV,
    content: echoArray,
    mode: "smart",
  });
  eq("copilot array user-echo → joined string prompt", echoParsed.prompt, "What is the\ncapital of Japan?");
  ok("copilot array user-echo prompt is a string", typeof echoParsed.prompt === "string");
  // The same array run through the emission chokepoint yields the same joined
  // string (defense-in-depth: any per-site parser that leaks an array is caught
  // here before the wire).
  eq(
    "coerceContentText copilot array user-echo → joined string",
    P.coerceContentText(echoArray),
    "What is the\ncapital of Japan?"
  );

  // --- server stream: received → startMessage → appendText×N → done → title ---
  const acc = P.makeCopilotAccumulator();
  const st = feedCopilot(acc, [
    { event: "received", conversationId: CONV, messageId: USER_MSG, createdAt: 1 },
    { event: "startMessage", conversationId: CONV, messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, partId: "p1", text: "**The" },
    { event: "appendText", messageId: ASSISTANT_MSG, partId: "p1", text: " capital** of Japan " },
    { event: "appendText", messageId: ASSISTANT_MSG, partId: "p1", text: "is **Tokyo**." },
    { event: "citation", messageId: ASSISTANT_MSG },
    { event: "partCompleted", messageId: ASSISTANT_MSG },
    { event: "done", messageId: ASSISTANT_MSG },
    { event: "titleUpdate", conversationId: CONV, title: "Capital of Japan: Tokyo" },
  ]);
  eq("copilot response assembled from appendText in order", st.text, "**The capital** of Japan is **Tokyo**.");
  eq("copilot conversation_id", st.conversationId, CONV);
  // message_id is the ASSISTANT id from startMessage (NOT the received/user id).
  eq("copilot message_id from startMessage", st.messageId, ASSISTANT_MSG);
  ok("copilot message_id is NOT the received/user id", st.messageId !== USER_MSG);
  eq("copilot title harvested from titleUpdate", st.title, "Capital of Japan: Tokyo");
  ok("copilot done flag set", st.done === true);

  // --- message_id fallback: `received` id used only when startMessage absent ---
  const accNoStart = P.makeCopilotAccumulator();
  const stNoStart = feedCopilot(accNoStart, [
    { event: "received", conversationId: CONV, messageId: USER_MSG },
    { event: "appendText", text: "Answer." },
    { event: "done" },
  ]);
  eq("copilot message_id falls back to received when no startMessage", stNoStart.messageId, USER_MSG);
  // ...and startMessage OVERRIDES a prior received id when it arrives.
  const accOverride = P.makeCopilotAccumulator();
  const stOverride = feedCopilot(accOverride, [
    { event: "received", messageId: USER_MSG },
    { event: "startMessage", messageId: ASSISTANT_MSG },
  ]);
  eq("copilot startMessage overrides received id", stOverride.messageId, ASSISTANT_MSG);

  // --- empty/absent mode stays empty (never invent a model name) ---
  const noMode = P.parseCopilotSendFrame({ event: "send", content: [{ type: "text", text: "hi" }] });
  eq("copilot absent mode → empty model", noMode.model, "");

  // --- malformed frames are tolerated (no throw, no state corruption) ---
  const accSafe = P.makeCopilotAccumulator();
  accSafe.feed(null);
  accSafe.feed({ event: "appendText" }); // no text field
  accSafe.feed({ event: "unknownEvent", text: "x" });
  eq("copilot tolerates malformed frames", accSafe.state.text, "");

  // --- appendText is STRICTLY scoped to the latched assistant message
  // (adversarial review r5-1): once startMessage latches the answer id, a
  // foreign-message appendText (a suggestion stream) AND an id-less appendText
  // are BOTH rejected — an intervention/suggestion stream can neither
  // contaminate nor pad the answer. (This test previously PINNED the unsafe
  // behavior, asserting the id-less " (id-less continues)" was appended.)
  const accScope = P.makeCopilotAccumulator();
  const stScope = feedCopilot(accScope, [
    { event: "startMessage", messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, text: "Real answer." },
    { event: "appendText", messageId: "sugg-msg-1", text: " SUGGESTED FOLLOWUP?" },
    { event: "appendText", text: " (id-less continues)" },
    { event: "done", messageId: ASSISTANT_MSG },
  ]);
  eq(
    "copilot foreign + id-less appendText both rejected once latched",
    stScope.text,
    "Real answer.",
  );

  // --- a SECOND startMessage (foreign message S) must NOT replace the latched
  // authoritative assistant id, and its appendText must not contaminate (r5-1).
  const accSecondStart = P.makeCopilotAccumulator();
  const stSecondStart = feedCopilot(accSecondStart, [
    { event: "startMessage", messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, text: "Answer body." },
    { event: "startMessage", messageId: "foreign-S" },
    { event: "appendText", messageId: "foreign-S", text: " FOREIGN" },
    { event: "done", messageId: ASSISTANT_MSG },
  ]);
  eq("copilot second startMessage does NOT replace latched id", stSecondStart.messageId, ASSISTANT_MSG);
  eq("copilot foreign second-message text rejected", stSecondStart.text, "Answer body.");

  // --- a FOREIGN `done` (different messageId) must NOT terminate the answer;
  // only the latched id's `done` seals it (r5-1).
  const accForeignDone = P.makeCopilotAccumulator();
  feedCopilot(accForeignDone, [
    { event: "startMessage", messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, text: "Part one" },
    { event: "done", messageId: "foreign-S" }, // foreign done — ignored
  ]);
  ok("copilot foreign done does not set done", accForeignDone.state.done === false);
  feedCopilot(accForeignDone, [
    { event: "appendText", messageId: ASSISTANT_MSG, text: " part two" },
    { event: "done", messageId: ASSISTANT_MSG }, // matching done — seals
  ]);
  eq("copilot answer continued past foreign done", accForeignDone.state.text, "Part one part two");
  ok("copilot matching done sets done", accForeignDone.state.done === true);

  // --- FREEZE after the matching `done`: trailing frames can't mutate the
  // answer/ids, but a trailing `titleUpdate` (cosmetic, arrives after done on
  // the wire) is still harvested (r5-1 / r5-5).
  const accFreeze = P.makeCopilotAccumulator();
  feedCopilot(accFreeze, [
    { event: "startMessage", messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, text: "Sealed." },
    { event: "done", messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, text: " LATE" }, // frozen — ignored
    { event: "titleUpdate", title: "A Title" }, // cosmetic — still allowed
  ]);
  eq("copilot frozen after done: late append ignored", accFreeze.state.text, "Sealed.");
  eq("copilot frozen after done: trailing title still harvested", accFreeze.state.title, "A Title");

  const accPre = P.makeCopilotAccumulator();
  const stPre = feedCopilot(accPre, [
    { event: "appendText", messageId: "early-msg", text: "Early text." },
  ]);
  eq("copilot pre-startMessage appendText still appends", stPre.text, "Early text.");

  // --- fully id-less stream (no startMessage): the received-fallback path — an
  // id-less appendText + id-less done still capture + terminate (r5-1 leniency
  // when the answer scope was never latched).
  const accIdless = P.makeCopilotAccumulator();
  const stIdless = feedCopilot(accIdless, [
    { event: "received", messageId: USER_MSG },
    { event: "appendText", text: "Id-less answer." },
    { event: "done" },
  ]);
  eq("copilot id-less stream captures answer", stIdless.text, "Id-less answer.");
  ok("copilot id-less stream terminates", stIdless.done === true);

  // --- empty-string startMessage.messageId must NOT latch away the
  // `received` fallback (review LOW-1).
  const accEmptyStart = P.makeCopilotAccumulator();
  const stEmptyStart = feedCopilot(accEmptyStart, [
    { event: "startMessage", messageId: "" },
    { event: "received", messageId: USER_MSG },
  ]);
  eq(
    "copilot empty startMessage id leaves received fallback usable",
    stEmptyStart.messageId,
    USER_MSG,
  );

  // --- conversationId is first-wins (r5-2): a LATE frame from a different
  // (completed) turn must not clobber the established conversation id.
  const accConvFirst = P.makeCopilotAccumulator();
  feedCopilot(accConvFirst, [
    { event: "received", conversationId: CONV, messageId: USER_MSG },
    { event: "startMessage", conversationId: CONV, messageId: ASSISTANT_MSG },
    { event: "titleUpdate", conversationId: "OTHER-CONV", title: "late foreign" },
  ]);
  eq("copilot conversationId first-wins vs late foreign frame", accConvFirst.state.conversationId, CONV);

  // --- (re-review MED) an ID-LESS `done` must NOT freeze a LATCHED answer:
  // once startMessage latches the assistant id, an unrelated id-less `done`
  // (an intervention/suggestion stream terminator) is ignored, so subsequent
  // matching-id text is still appended and the answer emits once, in full,
  // only on the matching-id `done`.
  const accIdlessDone = P.makeCopilotAccumulator();
  const stIdlessDone = feedCopilot(accIdlessDone, [
    { event: "startMessage", messageId: ASSISTANT_MSG },
    { event: "appendText", messageId: ASSISTANT_MSG, text: "First half " },
    { event: "done" }, // id-less done — must be IGNORED (answer is latched)
    { event: "appendText", messageId: ASSISTANT_MSG, text: "second half." },
    { event: "done", messageId: ASSISTANT_MSG }, // matching done — seals
  ]);
  ok("copilot id-less done does NOT freeze a latched answer", stIdlessDone.text === "First half second half.");
  ok("copilot latched answer sealed only by matching-id done", stIdlessDone.done === true);

  // --- (re-review HIGH) latchedAnswerId() exposes the answer-scoped id so the
  // transport can quarantine an INTERRUPTED turn's stragglers. It reflects the
  // startMessage id, the first-identified appendText id when no startMessage,
  // and stays "" for a never-latched (id-less) stream — even after `received`
  // seeds state.messageId with the user-echo fallback.
  const accLatchStart = P.makeCopilotAccumulator();
  accLatchStart.feed({ event: "startMessage", messageId: ASSISTANT_MSG });
  eq("copilot latchedAnswerId reflects startMessage id", accLatchStart.latchedAnswerId(), ASSISTANT_MSG);

  const accLatchAppend = P.makeCopilotAccumulator();
  accLatchAppend.feed({ event: "appendText", messageId: "early-msg", text: "x" });
  eq("copilot latchedAnswerId latches first identified appendText", accLatchAppend.latchedAnswerId(), "early-msg");

  const accLatchNone = P.makeCopilotAccumulator();
  eq("copilot latchedAnswerId empty before any latch", accLatchNone.latchedAnswerId(), "");
  accLatchNone.feed({ event: "received", messageId: USER_MSG });
  accLatchNone.feed({ event: "appendText", text: "id-less" });
  eq("copilot latchedAnswerId stays empty for a never-latched stream", accLatchNone.latchedAnswerId(), "");
  ok("copilot latchedAnswerId is NOT the received/user fallback id", accLatchNone.latchedAnswerId() !== USER_MSG);

  // --- (re-review HIGH #2) RECEIVED-ONLY interruption window ------------------
  // The transport (content-main beginTurn) creates the NEXT accumulator with
  // deferLatchUntilReceived when a prior turn was interrupted after its
  // `received` echo but before ANY assistant id latched — the frames carry no
  // id we could quarantine, so a stale foreign startMessage/appendText would
  // otherwise latch (and contaminate) the fresh turn. These pin the pure
  // accumulator contract the fix rests on.

  // The two signals beginTurn reads to decide deferLatch: a prior turn that
  // saw a received but never latched an assistant id.
  const accRcvOnly = P.makeCopilotAccumulator();
  accRcvOnly.feed({ event: "received", messageId: USER_MSG });
  ok("received-only: sawReceivedEcho true", accRcvOnly.sawReceivedEcho() === true);
  eq("received-only: latchedAnswerId empty (no assistant id yet)", accRcvOnly.latchedAnswerId(), "");

  // deferLatch: a stale foreign startMessage / appendText / done arriving
  // BEFORE this turn's own received must NOT latch, append, or terminate.
  const accDefer = P.makeCopilotAccumulator({ deferLatchUntilReceived: true });
  accDefer.feed({ event: "startMessage", messageId: "assistant-A-orphan" });
  eq("deferLatch: orphan startMessage before own received does NOT latch", accDefer.latchedAnswerId(), "");
  accDefer.feed({ event: "appendText", messageId: "assistant-A-orphan", text: "A ANSWER" });
  eq("deferLatch: orphan appendText before own received is dropped", accDefer.state.text, "");
  accDefer.feed({ event: "done", messageId: "assistant-A-orphan" });
  ok("deferLatch: orphan done before own received does NOT terminate", accDefer.state.done === false);
  // this turn's OWN received arrives → latching re-enabled for its own frames.
  accDefer.feed({ event: "received", messageId: USER_MSG });
  accDefer.feed({ event: "startMessage", messageId: ASSISTANT_MSG });
  eq("deferLatch: own startMessage after own received latches", accDefer.latchedAnswerId(), ASSISTANT_MSG);
  accDefer.feed({ event: "appendText", messageId: ASSISTANT_MSG, text: "B answer." });
  accDefer.feed({ event: "appendText", messageId: "assistant-A-orphan", text: " STRAGGLER" });
  accDefer.feed({ event: "done", messageId: ASSISTANT_MSG });
  eq("deferLatch: captures ONLY this turn's answer", accDefer.state.text, "B answer.");
  eq("deferLatch: messageId is this turn's assistant id", accDefer.state.messageId, ASSISTANT_MSG);
  ok("deferLatch: terminates on its OWN done", accDefer.state.done === true);

  // deferLatch mode with normal received-first ordering behaves like a normal
  // turn — the guard never fires because received precedes startMessage.
  const accDeferNormal = P.makeCopilotAccumulator({ deferLatchUntilReceived: true });
  accDeferNormal.feed({ event: "received", messageId: USER_MSG });
  accDeferNormal.feed({ event: "startMessage", messageId: ASSISTANT_MSG });
  accDeferNormal.feed({ event: "appendText", messageId: ASSISTANT_MSG, text: "Hi." });
  accDeferNormal.feed({ event: "done", messageId: ASSISTANT_MSG });
  eq("deferLatch normal-order: full answer captured", accDeferNormal.state.text, "Hi.");
  eq("deferLatch normal-order: assistant id", accDeferNormal.state.messageId, ASSISTANT_MSG);

  // (b) appendText-latched-then-straggler: with no startMessage, the first
  // IDENTIFIED appendText PROMOTES state.messageId to the assistant id (so the
  // transport's done-path rememberPriorMsgId records the ASSISTANT id, not the
  // stale `received` user id — the RELATED bug in the finding).
  const accPromote = P.makeCopilotAccumulator();
  accPromote.feed({ event: "received", messageId: USER_MSG });
  accPromote.feed({ event: "appendText", messageId: ASSISTANT_MSG, text: "Answer via append." });
  eq("appendText-latch: latchedAnswerId is the assistant id", accPromote.latchedAnswerId(), ASSISTANT_MSG);
  eq("appendText-latch: state.messageId PROMOTED to assistant id (b)", accPromote.state.messageId, ASSISTANT_MSG);
  ok("appendText-latch: state.messageId is NOT the received user id", accPromote.state.messageId !== USER_MSG);
  accPromote.feed({ event: "done", messageId: ASSISTANT_MSG });
  eq("appendText-latch: terminates on the assistant-id done", accPromote.state.text, "Answer via append.");

  // Default (non-defer) accumulator still latches a startMessage-FIRST stream
  // with NO preceding received — the isolated-accumulator contract every
  // pre-existing test relies on is intact.
  const accNoDeferStart = P.makeCopilotAccumulator();
  accNoDeferStart.feed({ event: "startMessage", messageId: ASSISTANT_MSG });
  eq("non-defer: startMessage-first still latches immediately", accNoDeferStart.latchedAnswerId(), ASSISTANT_MSG);
  accNoDeferStart.feed({ event: "appendText", messageId: ASSISTANT_MSG, text: "ok" });
  eq("non-defer: startMessage-first captures", accNoDeferStart.state.text, "ok");

  // --- (re-review HIGH #2) DROP-OVER-CORRUPT: contested turns never emit ------
  // The Copilot wire has NO per-generation correlator (parseCopilotSendFrame
  // harvests only conversationId/content/mode; `received` an unrelated user id;
  // `startMessage` an unrelated assistant id), so a turn begun while a prior
  // turn's frames are still inbound cannot PROVE which assistant frames are its
  // own — and arrival order is not a sound proxy. The prior wave's sawReceived
  // latch is REPLACED as the correctness guarantee by a DROP: a contested turn
  // does not emit (a deliberate missed capture over a misattributed one). These
  // pin the pure policy AND a transport simulation of content-main's WS loop.

  // copilotTurnDisposition — the drop decision from the PRIOR turn's state.
  eq(
    "disposition: prior already emitted → not contested",
    JSON.stringify(P.copilotTurnDisposition({ emitted: true, latchedId: "x", sawReceived: true })),
    JSON.stringify({ quarantineId: "", contested: false }),
  );
  eq(
    "disposition: prior latched assistant id (clean interrupt) → quarantine, not contested",
    JSON.stringify(P.copilotTurnDisposition({ emitted: false, latchedId: "asst-A", sawReceived: true })),
    JSON.stringify({ quarantineId: "asst-A", contested: false }),
  );
  eq(
    "disposition: received-only window → CONTESTED (drop)",
    JSON.stringify(P.copilotTurnDisposition({ emitted: false, latchedId: "", sawReceived: true })),
    JSON.stringify({ quarantineId: "", contested: true }),
  );
  eq(
    "disposition: clean prior (no received, no latch) → nothing",
    JSON.stringify(P.copilotTurnDisposition({ emitted: false, latchedId: "", sawReceived: false })),
    JSON.stringify({ quarantineId: "", contested: false }),
  );
  eq(
    "disposition: null prior → safe default",
    JSON.stringify(P.copilotTurnDisposition(null)),
    JSON.stringify({ quarantineId: "", contested: false }),
  );

  // copilotShouldEmit — done + not-yet-emitted + NOT contested + NOT tainted.
  ok("shouldEmit: done + fresh + uncontested → emit", P.copilotShouldEmit({ done: true, emitted: false, contested: false }) === true);
  ok("shouldEmit: contested → DROP (no emit)", P.copilotShouldEmit({ done: true, emitted: false, contested: true }) === false);
  ok("shouldEmit: not done → no emit", P.copilotShouldEmit({ done: false, emitted: false, contested: false }) === false);
  ok("shouldEmit: already emitted → no re-emit", P.copilotShouldEmit({ done: true, emitted: true, contested: false }) === false);
  ok("shouldEmit: null → no emit", P.copilotShouldEmit(null) === false);
  // wave-5 STICKY taint: a socket-tainted turn DROPS even when it looks
  // otherwise emittable (done + fresh + uncontested).
  ok("shouldEmit: tainted socket → DROP even if uncontested", P.copilotShouldEmit({ done: true, emitted: false, contested: false, tainted: true }) === false);
  ok("shouldEmit: absent tainted flag = falsy → unaffected", P.copilotShouldEmit({ done: true, emitted: false, contested: false }) === true);

  // copilotTaintNext — the pure per-socket sticky-taint transition. Sets on any
  // contested disposition; OR-only (never clears); safe on a null disposition.
  ok("taintNext: clean stays clean", P.copilotTaintNext(false, { contested: false }) === false);
  ok("taintNext: a contested turn taints the socket", P.copilotTaintNext(false, { contested: true }) === true);
  ok("taintNext: STICKY — a later uncontested turn stays tainted", P.copilotTaintNext(true, { contested: false }) === true);
  ok("taintNext: stays tainted through a null disposition", P.copilotTaintNext(true, null) === true);
  ok("taintNext: clean + null disposition stays clean", P.copilotTaintNext(false, null) === false);

  // Transport SIMULATION: mirror content-main's WS loop (beginTurn on `send`,
  // per-socket sticky taint via copilotTaintNext, per-frame priorMsgIds
  // quarantine, copilotShouldEmit gate) over the PURE helpers so the end-to-end
  // drop-over-corrupt behavior is unit-tested without the DOM. This is the EXACT
  // structure content-main.js runs — one instance == one socket/tap, so a fresh
  // instance models a socket reconnect (taint clears naturally).
  function makeCopilotTransport() {
    let acc = P.makeCopilotAccumulator();
    let prompt = "";
    let emitted = false;
    let contested = false;
    let socketTainted = false; // wave-5: sticky per-socket taint
    const priorMsgIds = new Set();
    const emits = [];
    function begin(capturedPrompt) {
      const disp = P.copilotTurnDisposition({
        emitted,
        latchedId: acc.latchedAnswerId(),
        sawReceived: acc.sawReceivedEcho(),
      });
      if (disp.quarantineId) priorMsgIds.add(disp.quarantineId);
      socketTainted = P.copilotTaintNext(socketTainted, disp);
      prompt = capturedPrompt;
      acc = P.makeCopilotAccumulator({ deferLatchUntilReceived: disp.contested || socketTainted });
      contested = disp.contested;
      emitted = false;
    }
    function feed(f) {
      if (f && typeof f.messageId === "string" && f.messageId && priorMsgIds.has(f.messageId)) return;
      acc.feed(f);
      if (P.copilotShouldEmit({ done: acc.state.done, emitted, contested, tainted: socketTainted })) {
        emitted = true;
        priorMsgIds.add(acc.state.messageId);
        emits.push({ prompt, text: acc.state.text, messageId: acc.state.messageId });
      }
    }
    return { begin, feed, emits, tainted: () => socketTainted, accState: () => acc.state };
  }

  // (1) THE FINDING'S SCENARIO: A reaches received-only; B is sent; B's own
  // received arrives; THEN A's DELAYED startMessage/appendText/done arrive. The
  // prior wave would latch A's frames under B (sawReceived was true) and emit
  // A's answer with B's prompt. Now B is CONTESTED → it DROPS. No emit at all.
  const T1 = makeCopilotTransport();
  T1.begin("PROMPT A");
  T1.feed({ event: "received", messageId: "user-A" }); // A: received-only window
  T1.begin("PROMPT B"); // B sent while A is interrupted → B contested
  T1.feed({ event: "received", messageId: "user-B" }); // B's own received
  T1.feed({ event: "startMessage", messageId: "asst-A" }); // A's DELAYED frames…
  T1.feed({ event: "appendText", messageId: "asst-A", text: "A's ANSWER" });
  T1.feed({ event: "done", messageId: "asst-A" });
  eq("HIGH#2: contested turn DROPS — no emit at all", T1.emits.length, 0);
  ok(
    "HIGH#2: A's answer never paired with B's prompt (no B-under-A, no A-under-B)",
    T1.emits.findIndex((e) => e.text.indexOf("A's ANSWER") !== -1) === -1,
  );

  // (2) NORMAL single turn — unaffected: emits its own prompt+answer once.
  const T2 = makeCopilotTransport();
  T2.begin("PROMPT");
  T2.feed({ event: "received", messageId: "user-1" });
  T2.feed({ event: "startMessage", messageId: "asst-1" });
  T2.feed({ event: "appendText", messageId: "asst-1", text: "Answer." });
  T2.feed({ event: "done", messageId: "asst-1" });
  eq("HIGH#2: normal single turn emits once", T2.emits.length, 1);
  eq("HIGH#2: normal turn pairs its OWN prompt+answer", T2.emits[0].text, "Answer.");
  eq("HIGH#2: normal turn prompt", T2.emits[0].prompt, "PROMPT");

  // (3) BACK-TO-BACK clean turns — both emit, correctly paired.
  const T3 = makeCopilotTransport();
  T3.begin("P1");
  T3.feed({ event: "received", messageId: "u1" });
  T3.feed({ event: "startMessage", messageId: "a1" });
  T3.feed({ event: "appendText", messageId: "a1", text: "One." });
  T3.feed({ event: "done", messageId: "a1" });
  T3.begin("P2");
  T3.feed({ event: "received", messageId: "u2" });
  T3.feed({ event: "startMessage", messageId: "a2" });
  T3.feed({ event: "appendText", messageId: "a2", text: "Two." });
  T3.feed({ event: "done", messageId: "a2" });
  eq("HIGH#2: back-to-back clean turns both emit", T3.emits.length, 2);
  eq("HIGH#2: turn1 answer paired", T3.emits[0].text, "One.");
  eq("HIGH#2: turn2 answer paired", T3.emits[1].text, "Two.");
  eq("HIGH#2: turn2 prompt paired", T3.emits[1].prompt, "P2");

  // (4) CLEAN-INTERRUPT (prior latched an assistant id) — unaffected: the prior
  // id is quarantined, its straggler dropped, and the SECOND turn emits clean.
  const T4 = makeCopilotTransport();
  T4.begin("P1");
  T4.feed({ event: "received", messageId: "u1" });
  T4.feed({ event: "startMessage", messageId: "a1" }); // A latches an assistant id
  T4.feed({ event: "appendText", messageId: "a1", text: "partial" });
  T4.begin("P2"); // user interrupted (no done) → clean interrupt, a1 quarantined
  T4.feed({ event: "received", messageId: "u2" });
  T4.feed({ event: "appendText", messageId: "a1", text: " STRAGGLER" }); // A's late frame → dropped
  T4.feed({ event: "startMessage", messageId: "a2" });
  T4.feed({ event: "appendText", messageId: "a2", text: "Second answer." });
  T4.feed({ event: "done", messageId: "a2" });
  eq("HIGH#2: clean-interrupt emits only the second turn", T4.emits.length, 1);
  eq("HIGH#2: clean-interrupt second answer clean (no straggler)", T4.emits[0].text, "Second answer.");
  eq("HIGH#2: clean-interrupt second turn NOT contested (prompt paired)", T4.emits[0].prompt, "P2");

  // (5) STICKY TAINT (wave-5, the DEFINITIVE close): once a socket has had a
  // contested turn, it is DROP-ONLY for ALL later turns — even one that LOOKS
  // uncontested because the contested turn latched an id. The prior wave
  // "recovered" turn C here; that recovery was the bug (below reproduces the
  // exact corruption it enabled). Now C DROPS too.
  const T5 = makeCopilotTransport();
  T5.begin("PA");
  T5.feed({ event: "received", messageId: "uA" }); // A: received-only interruption
  T5.begin("PB"); // B contested → socket TAINTED for good
  ok("HIGH#2: socket tainted after B becomes contested", T5.tainted() === true);
  T5.feed({ event: "received", messageId: "uB" });
  T5.feed({ event: "startMessage", messageId: "aB" }); // B latches an id (aB)
  T5.feed({ event: "appendText", messageId: "aB", text: "B answer" });
  T5.feed({ event: "done", messageId: "aB" });
  eq("HIGH#2: contested B dropped", T5.emits.length, 0);
  T5.begin("PC"); // C: B latched aB so disposition says NOT contested…
  eq("HIGH#2: C disposition alone is uncontested (the wave-4 trap)", T5.emits.length, 0);
  ok("HIGH#2: …but the socket stays tainted (sticky)", T5.tainted() === true);
  T5.feed({ event: "received", messageId: "uC" });
  T5.feed({ event: "startMessage", messageId: "aC" });
  T5.feed({ event: "appendText", messageId: "aC", text: "C answer" });
  T5.feed({ event: "done", messageId: "aC" });
  eq("HIGH#2: tainted socket drops C too (no false recovery)", T5.emits.length, 0);

  // (6) THE EXACT WAVE-4 CORRUPTION REPRO: A received-only → B contested (latches
  // a DIFFERENT id than its real answer) → C treated as uncontested by the
  // disposition → B's LATE real-answer frames (an id NOT quarantined) fold into
  // C's accumulator. The old logic emitted {prompt:C, text:answerB}. With the
  // sticky taint, C's accumulator may still ACCUMULATE the foreign answer, but
  // the emit is SUPPRESSED — zero corrupt emits.
  const T6 = makeCopilotTransport();
  T6.begin("PROMPT A");
  T6.feed({ event: "received", messageId: "user-A" }); // A received-only
  T6.begin("PROMPT B"); // B contested → tainted
  T6.feed({ event: "received", messageId: "user-B" }); // B's own received
  T6.feed({ event: "startMessage", messageId: "stray-X" }); // B latches a STRAY id
  // B is interrupted before its real answer; C is sent.
  T6.begin("PROMPT C"); // disposition quarantines stray-X, says C uncontested…
  T6.feed({ event: "received", messageId: "user-C" }); // C's own received (satisfies deferLatch)
  // B's DELAYED real-answer frames now arrive — a DIFFERENT id (msg-B), NOT the
  // quarantined stray-X, so the priorMsgIds guard does NOT drop them; they fold
  // into C's fresh accumulator (this is precisely the wave-4 leak path).
  T6.feed({ event: "startMessage", messageId: "msg-B" });
  T6.feed({ event: "appendText", messageId: "msg-B", text: "ANSWER B" });
  T6.feed({ event: "done", messageId: "msg-B" });
  eq("HIGH#2: wave-4 repro — ZERO emits (corruption suppressed)", T6.emits.length, 0);
  ok(
    "HIGH#2: wave-4 repro — {prompt:C, text:answerB} never emitted",
    T6.emits.findIndex((e) => e.text.indexOf("ANSWER B") !== -1) === -1,
  );
  // Demonstrate the test is load-bearing: WITHOUT the sticky taint the SAME
  // frames WOULD have emitted the corrupt pair (the old copilotShouldEmit had no
  // `tainted` term). The accumulator DID capture the foreign answer under C's
  // prompt — only the taint gate stopped the emit.
  const corruptWouldEmit = P.copilotShouldEmit({ done: T6.accState().done, emitted: false, contested: false });
  ok("HIGH#2: wave-4 repro — old (taint-free) gate WOULD have emitted the corrupt pair", corruptWouldEmit === true);
  eq("HIGH#2: wave-4 repro — the leaked pair was answerB under promptC", T6.accState().text, "ANSWER B");

  // (7) PER-SOCKET isolation + RESYNC on reconnect: a taint on one socket never
  // touches another; a fresh transport (== a new WS tap after reconnect) starts
  // clean and emits normally. This is the ONLY taint-clear path.
  const tainted = makeCopilotTransport();
  tainted.begin("PA");
  tainted.feed({ event: "received", messageId: "uA" });
  tainted.begin("PB"); // contest → taint
  ok("HIGH#2: socket-1 tainted", tainted.tainted() === true);
  const fresh = makeCopilotTransport(); // reconnect = new tap = fresh state
  ok("HIGH#2: a fresh socket/tap starts UN-tainted", fresh.tainted() === false);
  fresh.begin("Q1");
  fresh.feed({ event: "received", messageId: "fu1" });
  fresh.feed({ event: "startMessage", messageId: "fa1" });
  fresh.feed({ event: "appendText", messageId: "fa1", text: "Fresh answer." });
  fresh.feed({ event: "done", messageId: "fa1" });
  eq("HIGH#2: fresh socket emits its own turn normally", fresh.emits.length, 1);
  eq("HIGH#2: fresh socket answer paired", fresh.emits[0].text, "Fresh answer.");
  eq("HIGH#2: fresh socket prompt paired", fresh.emits[0].prompt, "Q1");
  ok("HIGH#2: the tainted socket is unchanged by the fresh one", tainted.tainted() === true);

  // (8) A NEVER-INTERRUPTED socket runs MANY turns, all emitting, never tainted.
  const clean = makeCopilotTransport();
  for (let i = 1; i <= 4; i++) {
    clean.begin("PMT" + i);
    clean.feed({ event: "received", messageId: "cu" + i });
    clean.feed({ event: "startMessage", messageId: "ca" + i });
    clean.feed({ event: "appendText", messageId: "ca" + i, text: "ans" + i });
    clean.feed({ event: "done", messageId: "ca" + i });
  }
  eq("HIGH#2: never-interrupted socket emits all 4 turns", clean.emits.length, 4);
  ok("HIGH#2: never-interrupted socket never tainted", clean.tainted() === false);
  eq("HIGH#2: never-interrupted pairing intact (turn 3)", clean.emits[2].prompt, "PMT3");
  eq("HIGH#2: never-interrupted pairing intact (turn 3 text)", clean.emits[2].text, "ans3");

  // --- copilotJoinContent + rewriteCopilotContent: single-object + structure
  // preservation (adversarial review r5-3). ------------------------------------
  // A NON-array single {type:text} object yields its prompt (was "" before).
  eq(
    "copilotJoinContent single object → text",
    P.copilotJoinContent({ type: "text", text: "single part prompt" }),
    "single part prompt",
  );
  // A nested parts container is walked.
  eq(
    "copilotJoinContent nested parts container",
    P.copilotJoinContent({ parts: [{ type: "text", text: "a" }, { type: "text", text: "b" }] }),
    "a\nb",
  );
  // Structure-preserving redaction of MIXED multimodal content: ordering +
  // every non-text part's metadata survive; only text fields change.
  const REDACT = (s) => s.replace(/SECRET/g, "[X]");
  const mixed = [
    { type: "image", url: "blob:one", meta: { w: 10 } },
    { type: "text", text: "before SECRET after", partId: "p7" },
    { type: "image", url: "blob:two", meta: { w: 20 } },
  ];
  const rewritten = P.rewriteCopilotContent(mixed, REDACT);
  eq("rewrite preserves array length", rewritten.length, 3);
  ok("rewrite keeps image1 at index 0 (byte-equal)", JSON.stringify(rewritten[0]) === JSON.stringify(mixed[0]));
  ok("rewrite keeps image2 at index 2 (byte-equal)", JSON.stringify(rewritten[2]) === JSON.stringify(mixed[2]));
  eq("rewrite redacts the text part in place", rewritten[1].text, "before [X] after");
  eq("rewrite preserves text part metadata (partId)", rewritten[1].partId, "p7");
  ok("rewrite does not mutate the original array", mixed[1].text === "before SECRET after");
  // String content passes through the redactor directly.
  eq("rewrite string content", P.rewriteCopilotContent("hi SECRET", REDACT), "hi [X]");
  // Single-object content is rewritten in place too.
  const singleRw = P.rewriteCopilotContent({ type: "text", text: "SECRET", partId: "z" }, REDACT);
  eq("rewrite single-object text", singleRw.text, "[X]");
  eq("rewrite single-object keeps metadata", singleRw.partId, "z");

  // --- capContentField: caps a content field + marks truncation (deferred #2).
  const big = "x".repeat(P.MAX_CONTENT_FIELD_BYTES + 5000);
  const capped = P.capContentField(big);
  ok("capContentField clamps within budget", capped.length <= P.MAX_CONTENT_FIELD_BYTES);
  ok("capContentField appends truncation marker", capped.endsWith(P.CONTENT_TRUNCATION_MARKER));
  eq("capContentField passes short text unchanged", P.capContentField("short"), "short");
  eq("capContentField non-string → empty", P.capContentField(null), "");

  // --- coerceContentText: the emission-boundary array→string normalizer that
  // guarantees the wire never carries a non-string prompt_text/response_text
  // (the daemon's browserchat.Parse unmarshals both into a Go string). Mirrors
  // the Go flexString decoder: string passthrough; array of strings/{text}
  // parts joined with "\n"; non-text parts dropped; junk → "".
  const coerceCases = [
    ["plain string passthrough", "already a string", "already a string"],
    ["empty string", "", ""],
    ["array of plain strings joined", ["a", "b", "c"], "a\nb\nc"],
    [
      "array of {text} parts joined",
      [{ type: "text", text: "one" }, { type: "text", text: "two" }],
      "one\ntwo",
    ],
    [
      "mixed string + {text} parts",
      ["lead", { text: "middle" }, "tail"],
      "lead\nmiddle\ntail",
    ],
    [
      "non-text parts dropped (image between text)",
      [{ type: "text", text: "before" }, { type: "image", url: "blob:x" }, { type: "text", text: "after" }],
      "before\nafter",
    ],
    ["single {text} object (non-array)", { type: "text", text: "solo" }, "solo"],
    ["nested content container", { content: [{ text: "n1" }, { text: "n2" }] }, "n1\nn2"],
    ["nested parts container", { parts: ["p1", "p2"] }, "p1\np2"],
    ["number → empty", 42, ""],
    ["null → empty", null, ""],
    ["undefined → empty", undefined, ""],
    ["bare object without text → empty", { foo: "bar" }, ""],
    ["array of junk → empty", [1, true, { foo: "bar" }], ""],
    // --- shared shape contract cross-cases (must mirror the Go side EXACTLY;
    // see TestParseFlexibleContentFields cross-shape cases in browserchat_test.go).
    ["top-level {text} object", { text: "real" }, "real"],
    ["array of {content:[{text}]} elements", [{ content: [{ text: "real" }] }], "real"],
    ["array of {parts:[...]} elements", [{ parts: ["real"] }], "real"],
    ["nested bare array (single)", [["real"]], "real"],
    ["nested bare arrays flattened", ["a", ["b", "c"], [["d"]]], "a\nb\nc\nd"],
    ["object .text as array of parts", { text: [{ text: "x" }, "y"] }, "x\ny"],
  ];
  for (const [name, input, want] of coerceCases) {
    eq("coerceContentText " + name, P.coerceContentText(input), want);
    ok(
      "coerceContentText " + name + " is a string",
      typeof P.coerceContentText(input) === "string"
    );
  }
  // capContentField funnels through coerceContentText, so an array reaching the
  // emission chokepoint becomes a joined STRING (never the raw array the daemon
  // rejects).
  eq(
    "capContentField coerces array user-echo to joined string",
    P.capContentField([{ type: "text", text: "capital of" }, { type: "text", text: "Japan?" }]),
    "capital of\nJapan?"
  );
  ok(
    "capContentField never returns an array",
    Array.isArray(P.capContentField([{ text: "x" }])) === false
  );

  // --- traversal bounds (finding 1: unbounded recursion/allocation is a DoS
  // vector). A pathologically deep nesting must NOT throw a RangeError, and a
  // bound hit must KEEP what was accumulated (truncate, never nuke to empty).
  // Build a ~5,000-deep nested-array wrapper around a leaf string.
  let deep = "buried";
  for (let i = 0; i < 5000; i++) deep = [deep];
  let deepOut;
  let threw = false;
  try {
    deepOut = P.coerceContentText(deep);
  } catch {
    threw = true;
  }
  ok("coerceContentText deep nesting does not throw", threw === false);
  // Past the depth bound the buried leaf is dropped, but the walk degrades to
  // "" rather than crashing — capture-something (here nothing survivable) beats
  // an exception that fails the whole turn.
  eq("coerceContentText deep nesting degrades to string", typeof deepOut, "string");

  // A huge flat array of text parts is node-bounded: it must NOT hang and must
  // return a non-empty joined prefix (kept, not nuked).
  const many = [];
  for (let i = 0; i < 100000; i++) many.push({ text: "p" });
  const manyOut = P.coerceContentText(many);
  ok("coerceContentText huge array returns a string", typeof manyOut === "string");
  ok("coerceContentText huge array keeps a non-empty prefix", manyOut.length > 0);
  ok(
    "coerceContentText huge array is node-bounded (does not join all 100k)",
    manyOut.split("\n").length <= 256
  );

  // A single leaf far larger than the byte budget is clamped (bounded near the
  // field cap), not dropped and not built in full.
  const overInput = "z".repeat(P.MAX_CONTENT_FIELD_BYTES * 2);
  const overBudget = P.coerceContentText(overInput);
  ok("coerceContentText clamps an over-budget leaf below its input", overBudget.length < overInput.length);
  ok("coerceContentText keeps the over-budget prefix (not nuked)", overBudget.length > 0);
  ok(
    "coerceContentText bounds an over-budget leaf near the field cap",
    overBudget.length <= P.MAX_CONTENT_FIELD_BYTES + 8192
  );
  // capContentField then trims to the field cap AND appends the truncation
  // marker (the marker path survives the coercion bound thanks to the slack).
  const cappedOver = P.capContentField(overInput);
  ok("capContentField clamps over-budget within the field cap", cappedOver.length <= P.MAX_CONTENT_FIELD_BYTES);
  ok("capContentField marks over-budget truncation", cappedOver.endsWith(P.CONTENT_TRUNCATION_MARKER));

  // --- finding 2: the LIVE copilot prompt path is bounded ---------------------
  // parseCopilotSendFrame → copilotJoinContent now routes through the bounded
  // coercer, so a hostile huge parts array can't be joined in full (CPU pin) and
  // a deeply-nested content/parts container can't blow the stack — the original
  // JS DoS class survived on exactly this path until the delegation.
  {
    const hugeParts = [];
    for (let i = 0; i < 100000; i++) hugeParts.push({ type: "text", text: "p" });
    let threw = false;
    let out;
    try {
      out = P.parseCopilotSendFrame({
        event: "send",
        conversationId: "c",
        content: hugeParts,
        mode: "smart",
      }).prompt;
    } catch {
      threw = true;
    }
    ok("copilot huge parts array does not throw", threw === false);
    ok("copilot huge parts prompt is a string", typeof out === "string");
    ok("copilot huge parts prompt is node-bounded (not all 100k joined)", out.split("\n").length <= 256);
    ok("copilot huge parts keeps a non-empty prefix (truncate, not nuke)", out.length > 0);

    // Deeply-nested content/parts containers must not overflow the stack.
    let deep = { type: "text", text: "buried" };
    for (let i = 0; i < 5000; i++) deep = { content: [deep] };
    let deepThrew = false;
    let deepOut;
    try {
      deepOut = P.copilotJoinContent(deep);
    } catch {
      deepThrew = true;
    }
    ok("copilot deep nesting does not throw", deepThrew === false);
    ok("copilot deep nesting degrades to a string", typeof deepOut === "string");
    // The pinned copilot shapes still coerce identically after the delegation.
    eq("copilot delegated array still joins", P.copilotJoinContent([{ type: "text", text: "a" }, { type: "text", text: "b" }]), "a\nb");
    eq("copilot delegated single object still yields text", P.copilotJoinContent({ type: "text", text: "solo" }), "solo");
  }

  // --- finding 3: the content budget is enforced in UTF-8 BYTES, not code units.
  {
    const utf8Bytes = (s) => Buffer.byteLength(s, "utf8");
    // 50 CJK chars = 50 code units but 150 UTF-8 bytes. Under a 100-BYTE cap a
    // code-unit check (50 <= 100) would pass it through UNTRUNCATED at 150 bytes
    // (the old bug); the byte check must truncate it to <= 100 bytes.
    const cjk = "字".repeat(50);
    ok("CJK code-unit length is under the byte cap", cjk.length < 100);
    ok("CJK UTF-8 byte length exceeds the byte cap", utf8Bytes(cjk) > 100);
    const cappedCjk = P.capContentField(cjk, 100);
    ok("capContentField enforces the cap in UTF-8 bytes", utf8Bytes(cappedCjk) <= 100);
    ok("capContentField keeps a non-empty CJK prefix", cappedCjk.length > 0);
    ok("capContentField marks the CJK truncation", cappedCjk.endsWith(P.CONTENT_TRUNCATION_MARKER));
    ok("capContentField never splits a CJK code point (valid UTF-8 round-trip)",
      Buffer.from(cappedCjk, "utf8").toString("utf8") === cappedCjk);

    // A full-size CJK field: with a code-unit budget a 2 MiB-code-unit CJK
    // prompt+response could serialize to ~12.6 MiB and blow past the daemon's
    // 8 MiB ingest cap (dropping the whole turn). Byte-bounding keeps each field
    // under the field cap, so two of them fit the ingest cap.
    const cjkField = "字".repeat(P.MAX_CONTENT_FIELD_BYTES); // 3 bytes/char → huge
    const cappedField = P.capContentField(cjkField);
    ok("capContentField CJK field is byte-bounded to the field cap", utf8Bytes(cappedField) <= P.MAX_CONTENT_FIELD_BYTES);
    ok("two capped CJK fields fit the daemon's 8 MiB ingest cap", 2 * utf8Bytes(cappedField) < 8 * 1024 * 1024);
  }

  // --- round-3 finding 2: the content budget is JSON-WIRE bytes, not raw UTF-8.
  // JSON.stringify EXPANDS backslashes/control chars/lone surrogates, so a
  // raw-UTF-8 budget let two 2 MiB escape-heavy fields serialize to >8 MiB and
  // the daemon's 8 MiB ingest cap then dropped the WHOLE turn. capContentField
  // must cap each field in its SERIALIZED size so the envelope stays ingestible.
  {
    const INGEST_CAP = 8 * 1024 * 1024;
    const wireBytes = (v) => Buffer.byteLength(JSON.stringify(v), "utf8");
    // Raw-UTF-8 budgets would pass these UNTRUNCATED: each backslash serializes
    // to 2 bytes ("\\"), each NUL to 6 (" ").
    const backslashHeavy = "\\".repeat(P.MAX_CONTENT_FIELD_BYTES);
    const nulHeavy = " ".repeat(P.MAX_CONTENT_FIELD_BYTES);
    ok("raw backslash field UNDER the field cap in raw length", backslashHeavy.length <= P.MAX_CONTENT_FIELD_BYTES);
    ok("but backslash field OVER the cap once serialized", wireBytes(backslashHeavy) > P.MAX_CONTENT_FIELD_BYTES);
    ok("and NUL field FAR over the cap once serialized", wireBytes(nulHeavy) > 5 * P.MAX_CONTENT_FIELD_BYTES);

    const prompt = P.capContentField(backslashHeavy);
    const response = P.capContentField(nulHeavy);
    ok("capContentField marks the backslash truncation", prompt.endsWith(P.CONTENT_TRUNCATION_MARKER));
    ok("capContentField marks the NUL truncation", response.endsWith(P.CONTENT_TRUNCATION_MARKER));
    // Each capped field's own serialized size honours the (wire) field budget.
    ok("capped backslash field serializes within the field cap", wireBytes(prompt) <= P.MAX_CONTENT_FIELD_BYTES + 64);
    ok("capped NUL field serializes within the field cap", wireBytes(response) <= P.MAX_CONTENT_FIELD_BYTES + 64);

    // Two worst-case escape-heavy fields + the JSON envelope stay under the cap.
    const envelope = {
      schema_version: 1,
      site: "copilot-web",
      conversation_id: "c1",
      message_id: "m1",
      granularity: "full",
      prompt_text: prompt,
      response_text: response,
    };
    ok(
      "two escape-heavy fields + envelope stay under the 8 MiB ingest cap",
      wireBytes(envelope) < INGEST_CAP
    );
  }

  // --- round-3 finding 3: copilotJoinContent uses copilot's OWN part predicate
  // (type "text"|undefined), NOT the generic coercer's any-object-with-.text.
  // A {type:"image",text:"SECRET"} part is non-text for rewriteCopilotContent
  // (left structurally in place, unredacted), so it must NOT leak into the
  // captured prompt via the join — otherwise capture and intervention disagree.
  {
    const imagePart = { type: "image", text: "SECRET", url: "blob:x" };
    // Copilot path EXCLUDES the image part's text.
    eq(
      "copilot join excludes a non-text {type:image,text} part",
      P.copilotJoinContent([{ type: "text", text: "keep" }, imagePart, { type: "text", text: "me" }]),
      "keep\nme"
    );
    eq(
      "copilot join excludes a lone non-text part → empty",
      P.copilotJoinContent([imagePart]),
      ""
    );
    eq(
      "copilot join excludes a single non-text object → empty",
      P.copilotJoinContent(imagePart),
      ""
    );
    // rewriteCopilotContent agrees: the image part is preserved verbatim (its
    // text is NOT redacted — it is a non-text part), proving the predicates
    // match. (If the join included it while rewrite skips it, capture and
    // intervention would be inconsistent — the exact bug this closes.)
    const REDACT2 = (s) => s.replace(/SECRET/g, "[X]");
    const rw = P.rewriteCopilotContent([imagePart], REDACT2);
    ok("rewrite leaves the non-text image part verbatim (unredacted)", rw[0].text === "SECRET");

    // A {type:undefined,text} part IS accepted on the copilot path (matches
    // copilotPartText's `type === undefined` branch).
    eq(
      "copilot join accepts a typeless {text} part",
      P.copilotJoinContent([{ text: "typeless" }]),
      "typeless"
    );

    // GENERIC path (non-copilot sites) is UNCHANGED: coerceContentText still
    // accepts ANY object carrying a .text, so the same image part's text is
    // included there. Only the copilot path narrows the predicate.
    eq(
      "generic coerceContentText still includes any-object .text (unchanged)",
      P.coerceContentText([imagePart]),
      "SECRET"
    );
  }

  // --- round-4 finding: copilotCollect's acceptance is EXACTLY co-extensive
  // with rewriteCopilotContent's reach. The collector must NEVER capture text
  // the redactor cannot reach — otherwise redact mode ships that text upstream
  // verbatim. Paired invariant, checked with the SAME fixture fed to BOTH
  // functions: captured ⇒ redactable — if copilotJoinContent surfaces "SECRET",
  // rewriteCopilotContent(fixture, redact) must contain NO "SECRET" anywhere in
  // its serialized output. Equivalently, a fixture the redactor leaves with a
  // verbatim "SECRET" MUST NOT contribute that "SECRET" to the capture.
  {
    const REDACT = (s) => s.replace(/SECRET/g, "[X]");
    const rewriteLeavesSecret = (fixture) =>
      JSON.stringify(P.rewriteCopilotContent(fixture, REDACT)).includes("SECRET");
    const captures = (fixture) => P.copilotJoinContent(fixture).includes("SECRET");

    // Shapes the redactor CANNOT reach → the collector MUST exclude them. These
    // are the exact leaks the round-4 finding calls out: a bare string inside an
    // array, and a doubly-nested container (rewriteCopilotContent descends only
    // ONE level of parts).
    const unredactable = [
      ["bare string in array", ["SECRET"]],
      ["double-nested content container", { content: [{ content: [{ type: "text", text: "SECRET" }] }] }],
      ["double-nested parts container", { parts: [{ parts: [{ type: "text", text: "SECRET" }] }] }],
      ["non-text {type:image,text} part in array", [{ type: "image", text: "SECRET" }]],
      ["single non-text object", { type: "image", text: "SECRET" }],
    ];
    for (const [name, fixture] of unredactable) {
      ok("rewrite leaves SECRET verbatim for " + name + " (unredactable)", rewriteLeavesSecret(fixture));
      ok("copilotCollect EXCLUDES the unredactable " + name, captures(fixture) === false);
      eq("copilotJoinContent drops the unredactable " + name + " → empty", P.copilotJoinContent(fixture), "");
    }

    // Shapes the redactor DOES reach (the previously-pinned accepted shapes) →
    // the collector captures AND the redactor fully scrubs: co-extensive from
    // the other side too.
    const redactable = [
      ["top-level string", "SECRET"],
      ["accepted {type:text} part in array", [{ type: "text", text: "SECRET" }]],
      ["single accepted part object", { type: "text", text: "SECRET" }],
      ["typeless {text} part in array", [{ text: "SECRET" }]],
      ["one-level content container", { content: [{ type: "text", text: "SECRET" }] }],
      ["one-level parts container", { parts: [{ type: "text", text: "SECRET" }] }],
    ];
    for (const [name, fixture] of redactable) {
      ok("copilotCollect captures the redactable " + name, captures(fixture));
      ok("rewrite fully scrubs the redactable " + name, rewriteLeavesSecret(fixture) === false);
    }

    // The universal paired invariant across EVERY fixture: never
    // captured-AND-left-verbatim — the exact capture/redaction inconsistency
    // this closes.
    for (const [name, fixture] of [...unredactable, ...redactable]) {
      ok(
        "co-extensive: not (captured && redactor leaves verbatim) for " + name,
        !(captures(fixture) && rewriteLeavesSecret(fixture))
      );
    }
  }
})();

// ====== capture_id passthrough across granularities (MED-1 + MED-3) ======
// content-isolated.applyGranularity is the isolated-world transform that maps a
// raw captured turn onto the wire payload. Under Node it exports the pure
// transforms instead of attaching the page listener, so we can assert the REAL
// function (not a replica) carries capture_id through EVERY granularity —
// including usage_only, where prompt/response content is stripped. That is the
// whole point: the daemon's id-less fallback key must survive the strip.
// content-isolated.resolveGranularity is the ZERO-CONFIG precedence: an
// explicit persisted user granularity, clamped by the daemon's cached ceiling
// (min by rank — never send more than the daemon stores), else the daemon
// ceiling, else the usage_only fail-closed default. The daemon's
// [browser].granularity_ceiling is the single lever; there is no options page.
(function resolveGranularityPrecedence() {
  const CI = require("./content-isolated.js");
  const R = CI.resolveGranularity;

  // Neither user nor daemon → fail-closed default.
  eq("neither → fallback", R(null, null, "usage_only"), "usage_only");
  eq("neither → fallback honors passed default", R(undefined, undefined, "redacted"), "redacted");
  eq("bad fallback collapses to usage_only", R(null, null, "bogus"), "usage_only");

  // Daemon only (the common zero-config case) → follow the daemon ceiling.
  eq("daemon only usage_only", R(null, "usage_only", "usage_only"), "usage_only");
  eq("daemon only redacted", R(null, "redacted", "usage_only"), "redacted");
  eq("daemon only full", R(null, "full", "usage_only"), "full");
  eq("daemon raises above default", R(undefined, "full", "usage_only"), "full");

  // User only (daemon unreachable / not yet cached) → honor the explicit user
  // choice (the daemon still clamps server-side at ingest).
  eq("user only full, no daemon", R("full", null, "usage_only"), "full");
  eq("user only redacted, no daemon", R("redacted", undefined, "usage_only"), "redacted");

  // Both present → min() by rank, never exceeding the daemon ceiling.
  eq("both: user full clamped by daemon usage_only", R("full", "usage_only", "usage_only"), "usage_only");
  eq("both: user full clamped by daemon redacted", R("full", "redacted", "usage_only"), "redacted");
  eq("both: user redacted under daemon full → user wins (lower)", R("redacted", "full", "usage_only"), "redacted");
  eq("both: user usage_only under daemon full → usage_only", R("usage_only", "full", "usage_only"), "usage_only");
  eq("both equal → that level", R("redacted", "redacted", "usage_only"), "redacted");

  // Unknown values collapse to the fallback.
  eq("unknown user with valid daemon → daemon", R("nonsense", "redacted", "usage_only"), "redacted");
  eq("unknown daemon with valid user → user", R("full", "nonsense", "usage_only"), "full");
  eq("both unknown → fallback", R("x", "y", "usage_only"), "usage_only");
})();

(async function captureIdGranularity() {
  const CI = require("./content-isolated.js");
  // The unset-storage DEFAULT granularity is a privacy contract: usage_only
  // means no content field is ever constructed unless the user EXPLICITLY
  // persists a higher level in chrome.storage.sync. The docs + CWS Limited
  // Use disclosure state this; a silent default flip is a consent
  // regression (GPT-5.6 review 2026-07-18, finding #1).
  eq("DEFAULTS.granularity is usage_only", CI.DEFAULTS.granularity, "usage_only");
  const rawTurn = {
    site: "chatgpt-web",
    conversation_id: "", // id-less turn — the case the fallback exists for
    capture_id: "cap_opaque_123",
    id_source: "none",
    message_id: "",
    model: "gpt-x",
    request_url: "/backend-api/conversation",
    prompt_text: "secret prompt content",
    response_text: "secret response content",
    latency_ms: 5,
    captured_at: "2026-07-17T00:00:00.000Z",
  };

  for (const g of ["usage_only", "redacted", "full"]) {
    const out = await CI.applyGranularity({ ...rawTurn }, g);
    eq("capture_id rides at " + g, out.capture_id, "cap_opaque_123");
    eq("id_source rides at " + g, out.id_source, "none");
    eq("conversation_id rides at " + g, out.conversation_id, "");
  }

  // usage_only MUST strip content while STILL carrying capture_id — the exact
  // collision/privacy case MED-1 fixes (content gone, opaque id remains).
  const uo = await CI.applyGranularity({ ...rawTurn }, "usage_only");
  eq("usage_only strips prompt_text", uo.prompt_text, undefined);
  eq("usage_only strips response_text", uo.response_text, undefined);
  ok("usage_only still carries capture_id", uo.capture_id === "cap_opaque_123");
  // full keeps content AND capture_id.
  const full = await CI.applyGranularity({ ...rawTurn }, "full");
  eq("full keeps prompt_text", full.prompt_text, "secret prompt content");
  eq("full keeps capture_id", full.capture_id, "cap_opaque_123");

  // A turn with no capture_id (older MAIN world) degrades to "" — additive,
  // never throws.
  const noCap = await CI.applyGranularity(
    { ...rawTurn, capture_id: undefined },
    "usage_only"
  );
  eq("missing capture_id → empty string", noCap.capture_id, "");
})()
  .then(() => {
    console.log("parsers.test.js: " + passed + " assertions passed");
  })
  .catch((err) => {
    console.error(err);
    process.exit(1);
  });
