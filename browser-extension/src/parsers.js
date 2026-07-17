// SPDX-License-Identifier: Apache-2.0
// Copyright (c) SuperBased. Part of SuperBased Observer.
//
// Per-site stream parsers for the browser-capture extension — the PURE half
// of content-main.js (the transport-patching IIFE stays there). Kept in its
// own UMD module, like tier1.js, so the SAME code loads into the MAIN
// content-script world (via the SBOParsers global) AND is `require`-able by
// the Node unit tests (src/parsers.test.js). ZERO extension-API / DOM
// dependencies.
//
// SITE = DATA / one parser factory per transport family (CLAUDE.md #3/#5):
// each site's request parser + streaming accumulator is a self-contained
// row the SITES table in content-main.js wires up; the transport-patching
// code is fully site-agnostic.
//
// The field paths encoded here come from July-2026 reverse-engineering of the
// live sites (see docs/plans/browser-extension-stream-shapes-2026-07-10.md).
// Items an operator's live capture must still confirm are flagged inline with
// `TODO(must-verify-live)`.
(function (root, factory) {
  "use strict";
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  } else {
    root.SBOParsers = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : self, function () {
  "use strict";

  // --- text helpers --------------------------------------------------------
  function joinParts(parts) {
    if (!Array.isArray(parts)) return "";
    return parts.map((p) => (typeof p === "string" ? p : "")).join("");
  }

  // unwrapSchematizedAnswer peels Perplexity's schematized-API double wrap.
  // LIVE-CONFIRMED 2026-07-10: with `params.use_schematized_api = true`
  // (default on the pplx_pro_upgraded build), FINAL.content.answer is ITSELF a
  // JSON string `{"answer":"…", …}` rather than plain text — so a single
  // `.answer` read returns the opaque wrapper `{"answer": "The last king…"}`
  // instead of the prose. Peel any `{answer:string}` wrapper (bounded depth;
  // plain-text answers never parse as an object with a string `.answer`, so
  // legacy shapes are untouched).
  function unwrapSchematizedAnswer(s) {
    let cur = typeof s === "string" ? s : "";
    for (let i = 0; i < 4; i++) {
      const t = cur.trim();
      if (t.length < 2 || t.charCodeAt(0) !== 0x7b /* { */) break;
      let inner;
      try {
        inner = JSON.parse(t);
      } catch {
        break;
      }
      if (inner && typeof inner.answer === "string") {
        cur = inner.answer;
        continue;
      }
      break;
    }
    return cur;
  }

  // makeLineReader buffers a partial trailing line across feed() calls. A
  // network chunk boundary can fall MID-FRAME, so splitting each chunk on
  // "\n" in isolation silently drops any frame straddling two chunks (an
  // assistant snapshot cut in half loses its id/model, deltas vanish). push()
  // returns only the COMPLETE lines and retains the remainder; flush() returns
  // the final unterminated line (a stream may end without a trailing newline,
  // e.g. `data: [DONE]`). Used by every SSE accumulator below.
  function makeLineReader() {
    let pending = "";
    return {
      push(chunk) {
        pending += chunk;
        const lines = pending.split("\n");
        pending = lines.pop(); // keep the last (possibly incomplete) segment
        return lines;
      },
      flush() {
        const last = pending;
        pending = "";
        return last;
      },
    };
  }

  // --- per-site request parsers --------------------------------------------
  // Each returns { prompt, model, conversationId } best-effort.
  function parseChatGPTRequest(bodyText) {
    const out = { prompt: "", model: "", conversationId: "" };
    if (!bodyText) return out;
    try {
      const j = JSON.parse(bodyText);
      // Model id: request body `model`. LIVE-CONFIRMED 2026-07-10: the
      // authenticated POST target carries the `/f/` segment
      // (`/backend-api/f/conversation`, with a `/prepare` debounce sibling) and
      // the model is at request `model` (observed "gpt-5-6-thinking").
      // TODO(must-verify-live): the server SSE echo key
      // (message.metadata.model_slug) is UNCONFIRMED — the captured turn was a
      // gpt-5-6-thinking one whose completion did NOT stream over this SSE leg
      // (see the conduit_token handoff note in the accumulator). The
      // accumulator still prefers model_slug when present.
      out.model = j.model || "";
      out.conversationId = j.conversation_id || "";
      const msgs = Array.isArray(j.messages) ? j.messages : [];
      for (let i = msgs.length - 1; i >= 0; i--) {
        const m = msgs[i];
        if (m && m.author && m.author.role === "user") {
          out.prompt = joinParts((m.content && m.content.parts) || []).trim();
          break;
        }
      }
    } catch {
      /* non-JSON — leave blank */
    }
    return out;
  }

  function parseClaudeRequest(bodyText) {
    // Claude.ai POST body on the completion endpoint: { prompt, model,
    // parent_message_uuid, ... }. LIVE-CONFIRMED 2026-07-10: prompt is a plain
    // string at `prompt`, model at `model` (observed value "claude-opus-4-8" —
    // the slug, no marketing alias). Other-shape probes retained defensively.
    const out = { prompt: "", model: "", conversationId: "" };
    if (!bodyText) return out;
    try {
      const j = JSON.parse(bodyText);
      out.model = j.model || "";
      out.prompt =
        (typeof j.prompt === "string" && j.prompt) ||
        (j.messages &&
          Array.isArray(j.messages) &&
          joinParts((j.messages[j.messages.length - 1] || {}).content || [])) ||
        "";
      // conversation id is in the URL path, resolved by the caller.
    } catch {
      /* ignore */
    }
    return out;
  }

  function parsePerplexityRequest(bodyText) {
    // Perplexity POST /rest/sse/perplexity_ask: { query_str, params:{
    // model_preference, mode, ... } }. Model lives under params.
    // LIVE-CONFIRMED 2026-07-10: prompt at `query_str`, model at
    // `params.model_preference` (observed value "pplx_pro_upgraded"). Top-level
    // fallbacks retained for older shapes.
    const out = { prompt: "", model: "", conversationId: "" };
    if (!bodyText) return out;
    try {
      const j = JSON.parse(bodyText);
      out.prompt =
        (typeof j.query_str === "string" && j.query_str) ||
        (typeof j.q === "string" && j.q) ||
        "";
      const params = j.params && typeof j.params === "object" ? j.params : {};
      out.model =
        params.model_preference ||
        params.model ||
        j.model_preference ||
        j.model ||
        "";
      out.conversationId =
        j.frontend_context_uuid || j.context_uuid || "";
    } catch {
      /* ignore */
    }
    return out;
  }

  function parseGeminiRequest(bodyText) {
    // Gemini's request is form-encoded (`f.req=<url-encoded JSON>&at=…`) — the
    // prompt is buried in a deeply-escaped RPC argument array and the model is
    // NOT in the body (it rides the `x-goog-ext-525001261-jspb` request HEADER,
    // an OPAQUE jspb array holding a session UUID, NOT a hex→model-name table —
    // there is no name table to build; the caller prefers a DOM model-picker
    // read for the model).
    //
    // LIVE-CONFIRMED 2026-07-11 (StreamGenerate capture): the form field
    // `f.req` decodes to `[null, "<inner-json-string>"]`; `JSON.parse(inner)`
    // is the RPC arg array whose `[0][0]` is the prompt text. The peel is
    // bounded and fail-soft to "" on ANY shape mismatch — Google churns these
    // array positions, and a wrong guess must degrade to an estimate, never
    // throw.
    const out = { prompt: "", model: "", conversationId: "" };
    if (!bodyText || typeof bodyText !== "string") return out;
    try {
      // Body is application/x-www-form-urlencoded; URLSearchParams applies the
      // '+' → space + percent-decode that form-encoding mandates. A literal
      // '+' inside the JSON was sent as %2B, so decoding is lossless.
      const reqRaw = new URLSearchParams(bodyText).get("f.req");
      if (!reqRaw) return out;
      const outer = JSON.parse(reqRaw);
      const innerStr =
        Array.isArray(outer) && typeof outer[1] === "string"
          ? outer[1]
          : typeof outer === "string"
          ? outer
          : "";
      if (!innerStr) return out;
      const inner = JSON.parse(innerStr);
      if (
        Array.isArray(inner) &&
        Array.isArray(inner[0]) &&
        typeof inner[0][0] === "string"
      ) {
        out.prompt = inner[0][0];
      }
    } catch {
      /* Google churns this RPC shape — fail soft to an empty prompt. */
    }
    return out;
  }

  // --- ChatGPT accumulator -------------------------------------------------
  // SSE `data: {json}` frames keyed o=op / p=path / v=value. Frame shapes
  // ([high] confidence, July-2026 reverse-engineering):
  //   (1) input_message echo of the USER turn — author.role != "assistant";
  //       EXCLUDED from the response text.
  //   (2) snapshot: {"p":"","o":"add","v":{"message":{id,author,content.parts,
  //       metadata},"conversation_id":...}} — carries the assistant message id
  //       + metadata.model_slug; parts are usually empty at stream start. The
  //       very first bootstrap frame omits o/p (default op "add" at path "").
  //   (3) patch batch: {"o":"patch","p":"","v":[{"p":"/message/content/parts/0",
  //       "o":"append","v":"text"}, ...]} — v is an ARRAY of sub-ops.
  //   (4) STICKY delta: {"v":"text"} with o AND p OMITTED — inherits o/p from
  //       the PREVIOUS frame. THIS IS THE #1 SILENT-FAILURE MODE: a parser
  //       that requires o+p on every frame drops most of the streamed text.
  // Response text = concat of every `v` string appended at a path containing
  // "content/parts/0" for the assistant message. Terminated by `data:[DONE]`.
  function makeChatGPTAccumulator() {
    const state = {
      text: "",
      conversationId: "",
      messageId: "",
      model: "",
      handoff: false,
    };
    // Sticky-frame inheritance: the last resolved op/path, carried into a
    // frame that omits o and/or p.
    let curOp = "";
    let curPath = "";
    let appendedAny = false;
    let snapshotText = "";

    function pathIsAssistantText(p) {
      return typeof p === "string" && p.indexOf("content/parts/0") !== -1;
    }

    // applyOp handles one resolved operation. Patch-batch sub-ops are routed
    // back through here with their own o/p/v.
    function applyOp(op, path, value) {
      if (Array.isArray(value)) {
        // patch batch: v is an array of sub-ops each carrying its own o/p/v.
        for (const sub of value) {
          if (!sub || typeof sub !== "object") continue;
          const so = "o" in sub ? sub.o : "append";
          const sp = "p" in sub ? sub.p : path;
          applyOp(so, sp, sub.v);
        }
        return;
      }
      if (value && typeof value === "object" && value.message) {
        // snapshot (initial full frame or an explicit add/replace of message).
        const m = value.message;
        if (value.conversation_id) state.conversationId = value.conversation_id;
        const role = m.author && m.author.role;
        if (role === "assistant") {
          if (m.id) state.messageId = m.id;
          // TODO(must-verify-live): server model echo key was
          // message.metadata.model_slug historically — UNCONFIRMED (the live
          // 2026-07-10 completion did not stream over this SSE leg; conduit).
          if (m.metadata && m.metadata.model_slug) {
            state.model = m.metadata.model_slug;
          }
          const joined = joinParts((m.content && m.content.parts) || []);
          if (joined.length > snapshotText.length) snapshotText = joined;
        }
        // author.role != assistant → input_message echo; excluded.
        return;
      }
      if (typeof value === "string" && pathIsAssistantText(path)) {
        state.text += value;
        appendedAny = true;
      }
    }

    const reader = makeLineReader();
    function handleLine(line) {
      const t = line.trim();
      if (!t.startsWith("data:")) return;
      const payload = t.slice(5).trim();
      if (payload === "" || payload === "[DONE]") return;
      // Pro/Thinking-tier bootstrap → a SECOND-leg WebSocket. [medium]
      if (payload.indexOf("stream_handoff") !== -1) {
        state.handoff = true;
        // PARTIALLY LIVE-CONFIRMED 2026-07-10: a gpt-5-6-thinking turn's first
        // leg (POST /backend-api/f/conversation/prepare) returns
        // {"status":"ok","conduit_token":"<JWT>"} — i.e. the real completion
        // streams over a SEPARATE "conduit" second leg keyed by that token, NOT
        // this SSE response. This grounds the two-leg model. STILL UNCONFIRMED:
        // the conduit/WS transport + `encoded_item` frame shape (no second-leg
        // capture yet). Until then a handoff turn records whatever the first
        // leg carried (may be empty for a pure-thinking response).
      }
      let ev;
      try {
        ev = JSON.parse(payload);
      } catch {
        return;
      }
      if (ev.conversation_id) state.conversationId = ev.conversation_id;
      // Resolve op/path with sticky inheritance from the previous frame.
      const op = "o" in ev ? ev.o : curOp;
      const path = "p" in ev ? ev.p : curPath;
      curOp = op;
      curPath = path;
      if ("v" in ev) applyOp(op, path, ev.v);
    }

    return {
      state,
      feed(chunk) {
        for (const line of reader.push(chunk)) handleLine(line);
      },
      finalize() {
        const last = reader.flush();
        if (last) handleLine(last);
        // Short responses can arrive as a snapshot only (no appends). Fall
        // back to the snapshot text when nothing was appended.
        if (!appendedAny && snapshotText.length > state.text.length) {
          state.text = snapshotText;
        }
      },
    };
  }

  // --- ChatGPT two-leg (handoff → resume) correlation ----------------------
  // LIVE-CONFIRMED 2026-07-11: a THINKING turn splits across two SSE POSTs.
  // Leg 1 (`/backend-api/f/conversation`) carries the prompt + request model
  // but its response is ONLY a `stream_handoff` + `[DONE]` — no answer text.
  // The answer streams on leg 2, a SECOND POST to
  // `/backend-api/f/conversation/resume` (body `{conversation_id, offset:0}`)
  // carrying the classic o/p/v deltas + the `model_slug` echo + the assistant
  // message id. A NON-thinking turn streams o/p/v directly on leg 1 with NO
  // handoff. These pure helpers let the transport layer (content-main.js)
  // decide, per finalized accumulator state, whether to buffer leg 1, merge a
  // resume leg, or emit directly — WITHOUT branching on a hostname.

  // isChatGPTResumePath reports whether a request path is the resume second
  // leg (matches both the `/f/` and non-`/f/` conversation bases).
  function isChatGPTResumePath(pathname) {
    return (
      typeof pathname === "string" && /\/conversation\/resume$/.test(pathname)
    );
  }

  // chatGPTIsHandoffPending reports whether a finalized leg is a thinking
  // turn's FIRST leg: a handoff was seen and no answer text arrived, so the
  // real answer is still coming on the resume leg. A leg with text (a normal
  // turn, or a resume leg) is NOT pending.
  function chatGPTIsHandoffPending(state) {
    return !!(state && state.handoff && !state.text);
  }

  // mergeChatGPTLegs folds a buffered leg-1 result (prompt + request model +
  // conversation id) with the resume leg's finalized accumulator state (answer
  // text + model_slug echo + assistant msg id) into one turn's fields. The
  // model_slug echo (leg 2) is preferred over the request model (leg 1) per
  // the ground-truth brief.
  function mergeChatGPTLegs(leg1, leg2State) {
    const a = leg1 || {};
    const b = leg2State || {};
    return {
      prompt: a.prompt || "",
      text: b.text || "",
      model: b.model || a.model || "",
      messageId: b.messageId || "",
      conversationId: b.conversationId || a.conversationId || "",
    };
  }

  // --- Claude.ai accumulator -----------------------------------------------
  // Anthropic Messages-API event model over SSE: message_start (model at
  // message.model) → content_block_start → content_block_delta (text via
  // delta.type "text_delta" → delta.text; reasoning via "thinking_delta" →
  // delta.thinking) → content_block_stop → message_delta → message_stop.
  // Response text = the text_delta.text run across every content block, in
  // stream order. thinking_delta is reasoning, NOT the visible response, so it
  // is not appended. LIVE-CONFIRMED 2026-07-10: the web stream carries NO usage
  // (no message.usage.input_tokens, no message_delta.usage.output_tokens) — the
  // usage reads below are kept only as a forward-compatible best-effort.
  function makeClaudeAccumulator() {
    const state = {
      text: "",
      conversationId: "",
      messageId: "",
      model: "",
      inputTokens: 0,
      outputTokens: 0,
    };
    const reader = makeLineReader();
    function handleLine(line) {
      const t = line.trim();
      if (!t.startsWith("data:")) return;
      const payload = t.slice(5).trim();
      if (payload === "" || payload === "[DONE]") return;
      let ev;
      try {
        ev = JSON.parse(payload);
      } catch {
        return;
      }
      if (ev.type === "content_block_delta" && ev.delta) {
        const dt = ev.delta.type;
        if (dt === "text_delta" && typeof ev.delta.text === "string") {
          state.text += ev.delta.text;
        } else if (dt === undefined && typeof ev.delta.text === "string") {
          // Defensive: an older shape without an explicit delta.type.
          state.text += ev.delta.text;
        }
        // thinking_delta (ev.delta.thinking) is intentionally ignored.
      } else if (ev.type === "message_start" && ev.message) {
        if (ev.message.id) state.messageId = ev.message.id;
        if (ev.message.model) state.model = ev.message.model;
        // LIVE-CONFIRMED 2026-07-10: the CONSUMER web /completion stream does
        // NOT expose usage — a full opus-4-8 turn carried no usage/token field
        // anywhere (message_start.usage absent; message_delta had no usage).
        // Tokens are therefore ESTIMATED (chars/4) downstream. We still read
        // usage when present so a future Anthropic change is picked up for free.
        if (
          ev.message.usage &&
          typeof ev.message.usage.input_tokens === "number"
        ) {
          state.inputTokens = ev.message.usage.input_tokens;
        }
      } else if (ev.type === "message_delta") {
        // usage.output_tokens is CUMULATIVE in message_delta — overwrite,
        // never accumulate.
        if (ev.usage && typeof ev.usage.output_tokens === "number") {
          state.outputTokens = ev.usage.output_tokens;
        }
      } else if (typeof ev.completion === "string") {
        state.text += ev.completion; // legacy single-field shape
      }
    }

    return {
      state,
      feed(chunk) {
        for (const line of reader.push(chunk)) handleLine(line);
      },
      finalize() {
        const last = reader.flush();
        if (last) handleLine(last);
      },
    };
  }

  // --- Perplexity accumulator ----------------------------------------------
  // SSE `event: message` frames + `data: {json}`, terminated by
  // `event: end_of_stream`. LIVE-CONFIRMED 2026-07-10: conversationId at
  // data.backend_uuid, messageId at data.uuid. The response text is nested
  // JSON-as-string: data.text is ITSELF a JSON string → parse → array of step
  // objects → the step with step_type=="FINAL" → its `content` (a JSON string
  // OR, on the schematized build, an object directly) → {answer, ...}. On the
  // schematized-API build the `.answer` is AGAIN a JSON string wrapper
  // (`{"answer":"…"}`), peeled by unwrapSchematizedAnswer. Decoding `text` only
  // once yields an opaque string, so a naive `data.text` read captures nothing.
  function makePerplexityAccumulator() {
    const state = { text: "", conversationId: "", messageId: "", model: "" };

    function decodeFinalAnswer(ev) {
      if (typeof ev.text === "string") {
        let steps;
        try {
          steps = JSON.parse(ev.text);
        } catch {
          steps = null;
        }
        if (Array.isArray(steps)) {
          for (const step of steps) {
            if (!step || step.step_type !== "FINAL") continue;
            if (typeof step.content === "string") {
              try {
                const fin = JSON.parse(step.content);
                if (fin && typeof fin.answer === "string") {
                  return unwrapSchematizedAnswer(fin.answer);
                }
              } catch {
                /* fall through to other shapes */
              }
            } else if (step.content && typeof step.content === "object") {
              // Schematized-API build (LIVE-CONFIRMED 2026-07-10): FINAL.content
              // arrives as an OBJECT directly and its `.answer` is a nested
              // JSON string — unwrap it to the prose.
              if (typeof step.content.answer === "string") {
                return unwrapSchematizedAnswer(step.content.answer);
              }
            }
          }
        }
      }
      // Fallbacks (older shapes): a plain answer/text field.
      if (typeof ev.answer === "string") return unwrapSchematizedAnswer(ev.answer);
      return "";
    }

    const reader = makeLineReader();
    function handleLine(line) {
      const t = line.trim();
      if (!t.startsWith("data:")) return;
      const payload = t.slice(5).trim();
      if (payload === "" || payload === "[DONE]") return;
      let ev;
      try {
        ev = JSON.parse(payload);
      } catch {
        return;
      }
      if (ev.backend_uuid) state.conversationId = ev.backend_uuid;
      if (ev.uuid && !state.messageId) state.messageId = ev.uuid;
      // Perplexity streams cumulative snapshots — keep the longest FINAL
      // answer seen.
      const answer = decodeFinalAnswer(ev);
      if (answer && answer.length > state.text.length) state.text = answer;
    }

    return {
      state,
      feed(chunk) {
        for (const line of reader.push(chunk)) handleLine(line);
      },
      finalize() {
        const last = reader.flush();
        if (last) handleLine(last);
      },
    };
  }

  // --- Gemini accumulator --------------------------------------------------
  // NOT SSE. The chat call is a batchexecute-style StreamGenerate RPC whose
  // response is length-prefixed JSON frames ([high] confidence, 2026):
  //   * strip the leading `)]}'` XSSI guard + surrounding whitespace;
  //   * then repeated `<length>\n<json_array>\n` frames where LENGTH is
  //     UTF-16 CODE UNITS — native JS String slicing is already UTF-16, so no
  //     byte<->code-unit conversion is needed;
  //   * each frame is an array of RPC entries; a chat entry is
  //     ["wrb.fr", <rpcid>, "<payload JSON string>", ...] — entry[2] is a JSON
  //     string → parse → part_json;
  //   * cid = part_json[1][0], rid = part_json[1][1], candidates = part_json[4];
  //     per candidate: rcid = candidate[0], done = candidate[8][0]==2,
  //     text = candidate[1][0] (fallback candidate[22][0]),
  //     reasoning = candidate[37][0][0].
  // Gemini re-sends the FULL cumulative text every frame (SNAPSHOT), so the
  // final answer is the longest text per rcid.
  // TODO(must-verify-live): confirm the candidate index layout (1 vs 22 for
  // text, 8[0]==2 done flag, 37 reasoning) against a raw StreamGenerate
  // envelope — Google churns these array positions.
  function makeGeminiAccumulator() {
    const state = { text: "", conversationId: "", messageId: "", model: "" };
    let buf = "";

    function stripXSSI(s) {
      const idx = s.indexOf(")]}'");
      if (idx !== -1) s = s.slice(idx + 4);
      return s.replace(/^\s+/, "");
    }

    // splitFrames walks the UTF-16-length-prefixed frames. Incomplete trailing
    // data is left unparsed (fail-soft).
    function splitFrames(s) {
      const frames = [];
      let pos = 0;
      while (pos < s.length) {
        const nl = s.indexOf("\n", pos);
        if (nl === -1) break;
        const lenStr = s.slice(pos, nl).trim();
        if (!/^\d+$/.test(lenStr)) {
          // Not a length line (blank / stray) — skip to the next line.
          pos = nl + 1;
          continue;
        }
        const len = parseInt(lenStr, 10);
        const start = nl + 1;
        const end = start + len; // UTF-16 code units — JS slice is UTF-16.
        if (end > s.length) break; // frame not fully buffered yet.
        const jsonStr = s.slice(start, end);
        try {
          frames.push(JSON.parse(jsonStr));
        } catch {
          /* skip malformed frame */
        }
        pos = end;
      }
      return frames;
    }

    function harvest(frames) {
      const byRcid = {};
      for (const frame of frames) {
        if (!Array.isArray(frame)) continue;
        for (const entry of frame) {
          if (!Array.isArray(entry) || entry[0] !== "wrb.fr") continue;
          const payloadStr = entry[2];
          if (typeof payloadStr !== "string") continue;
          let pj;
          try {
            pj = JSON.parse(payloadStr);
          } catch {
            continue;
          }
          if (Array.isArray(pj[1])) {
            if (typeof pj[1][0] === "string") state.conversationId = pj[1][0];
            if (typeof pj[1][1] === "string") state.messageId = pj[1][1];
          }
          const candidates = pj[4];
          if (!Array.isArray(candidates)) continue;
          for (const cand of candidates) {
            if (!Array.isArray(cand)) continue;
            const rcid = cand[0] || "_";
            let text = "";
            if (Array.isArray(cand[1]) && typeof cand[1][0] === "string") {
              text = cand[1][0];
            } else if (Array.isArray(cand[22]) && typeof cand[22][0] === "string") {
              text = cand[22][0];
            }
            if (text) byRcid[rcid] = text; // snapshot: latest wins per rcid.
          }
        }
      }
      let best = "";
      for (const k in byRcid) {
        if (byRcid[k].length > best.length) best = byRcid[k];
      }
      if (best) state.text = best;
    }

    return {
      state,
      feed(chunk) {
        buf += chunk;
      },
      finalize() {
        try {
          harvest(splitFrames(stripXSSI(buf)));
        } catch {
          // TODO(must-verify-live): if the structured decode yields nothing on
          // a real capture, the frame layout or candidate indices shifted —
          // re-confirm against a raw StreamGenerate envelope.
        }
      },
    };
  }

  return {
    joinParts,
    parseChatGPTRequest,
    parseClaudeRequest,
    parsePerplexityRequest,
    parseGeminiRequest,
    makeChatGPTAccumulator,
    makeClaudeAccumulator,
    makePerplexityAccumulator,
    makeGeminiAccumulator,
    isChatGPTResumePath,
    chatGPTIsHandoffPending,
    mergeChatGPTLegs,
  };
});
