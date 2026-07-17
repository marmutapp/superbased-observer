/*
 * SuperBased Observer — DevTools Console Capture Harness (diagnostic tool).
 *
 * WHAT THIS IS
 *   A PASSIVE, console-only ground-truth capture harness. Paste it into the
 *   DevTools Console on a logged-in AI-chatbot site (ChatGPT, Claude,
 *   Perplexity, Gemini), send ONE message, and it prints a JSON dump of the
 *   REAL authenticated request/response stream shape so we can tune the
 *   browser extension's per-site parsers (see
 *   internal/adapter/browserchat/browserchat.go CapturedTurn +
 *   browser-extension/src/content-main.js interceptors).
 *
 * SAFETY / PRIVACY
 *   - It makes NO network calls of its own. Nothing is exfiltrated. It only
 *     wraps window.fetch / XMLHttpRequest / WebSocket to OBSERVE requests to
 *     the current site's completion endpoint(s), and console.log()s a
 *     structural summary for YOU to copy.
 *   - It emits STRUCTURE (key shapes + JSON paths) plus SHORT truncated
 *     samples of the actual text. Those samples contain YOUR prompt/response
 *     text — REDACT the *_sample and *_raw fields before sharing. The parser
 *     only needs the field STRUCTURE and the JSON paths, not the content.
 *   - It restores the original functions on __sboStop().
 *
 * OPERATOR API (typed in the console)
 *   __sboDump()   print the captured turn(s) as JSON  (auto-prints on turn end)
 *   __sboStop()   restore original fetch/XHR/WebSocket, stop capturing
 *   __sboRaw(i)   print the FULL untruncated raw response buffer of capture i
 *                 (use for Gemini: paste the full REDACTED envelope so we can
 *                 find the text path)
 *   __SBO         the live state object (captures live here)
 *
 * This file is a dev tool. It is not shipped in the extension bundle, imports
 * nothing, and runs in a browser console (not Node) — but it is valid JS
 * (`node --check` passes).
 */
(function () {
  "use strict";

  if (typeof window === "undefined") {
    // Running under node --check / a non-browser: define nothing, just parse.
    return;
  }

  if (window.__SBO && window.__SBO.__armed) {
    console.warn(
      "[SBO] Harness already armed on this page. Run __sboStop() first to re-arm."
    );
    return;
  }

  // --- tunables ------------------------------------------------------------
  var MAX_VAL = 80; // truncate string VALUES in the structure dump to N chars
  var MAX_FRAMES = 10; // number of streamed frame samples kept for the dump
  var MAX_FRAME_CHARS = 240; // truncate each frame sample to N chars
  var MAX_RAW_BYTES = 4 * 1024 * 1024; // cap total buffered raw per turn
  var MAX_URL_LOG = 500; // cap entries kept in the URL-discovery log

  // --- per-site table ------------------------------------------------------
  // site: the models.Tool*Web tag; hostMatch: substring of location.hostname;
  // capturePaths / capturePathRe / capturePathContains: which endpoint(s) to
  // observe; transport: how the response streams. Mirrors content-main.js.
  var SITES = [
    {
      site: "chatgpt-web",
      transport: "sse",
      hostMatch: "chatgpt.com",
      capturePaths: [
        "/backend-api/conversation",
        "/backend-api/f/conversation",
      ],
    },
    {
      site: "claude-web",
      transport: "sse",
      hostMatch: "claude.ai",
      capturePathRe: /\/chat_conversations\/[^/]+\/completion$/,
    },
    {
      site: "perplexity-web",
      transport: "sse",
      hostMatch: "perplexity.ai",
      capturePaths: ["/rest/sse/perplexity_ask"],
    },
    {
      site: "gemini-web",
      transport: "batchexecute",
      hostMatch: "gemini.google.com",
      // The chat RPC streams over StreamGenerate; older/aux calls use
      // batchexecute. capturePathContains accepts a string OR an array of
      // needles (matchesCaptureURL ORs them). Both are length-prefixed
      // batchexecute-style envelopes → feedRaw + __sboRaw() give the frame body.
      capturePathContains: ["/BardChatUi/data/batchexecute", "StreamGenerate"],
      bestEffort: true,
    },
    {
      site: "copilot-web",
      transport: "websocket",
      hostMatch: "copilot.microsoft.com",
    },
  ];

  var SITE = null;
  for (var i = 0; i < SITES.length; i++) {
    if (location.hostname.indexOf(SITES[i].hostMatch) !== -1) {
      SITE = SITES[i];
      break;
    }
  }
  if (!SITE) {
    console.warn(
      "[SBO] This page (" +
        location.hostname +
        ") is not a recognized capture site. Supported: chatgpt.com, " +
        "claude.ai, perplexity.ai, gemini.google.com, copilot.microsoft.com."
    );
    return;
  }

  // --- state ---------------------------------------------------------------
  var STATE = {
    __armed: true,
    site: SITE.site,
    transport: SITE.transport,
    captures: [],
    originals: {},
    urlLog: [], // URL-discovery log: {seq, t, kind, method, url, matched}
  };
  window.__SBO = STATE;

  // Monotonic sequence + bounded logger for EVERY fetch / WebSocket / XHR the
  // page opens — URLs + methods ONLY, never bodies or headers. This is how we
  // discover the (unknown) ChatGPT conduit WebSocket host on a thinking turn:
  // send the message, then run __sboUrls() and read the ws:// host/path.
  var _urlSeq = 0;
  function logURL(kind, method, url, matched) {
    _urlSeq++;
    var entry = {
      seq: _urlSeq,
      t: new Date().toISOString(),
      kind: kind, // "fetch" | "ws" | "xhr"
      method: method || "",
      url: String(url),
      matched: !!matched,
    };
    if (STATE.urlLog.length < MAX_URL_LOG) {
      STATE.urlLog.push(entry);
    } else if (!STATE.urlLogTruncated) {
      STATE.urlLogTruncated = true;
    }
    return entry;
  }

  // --- helpers -------------------------------------------------------------
  function truncStr(s, n) {
    if (typeof s !== "string") return s;
    if (s.length <= n) return s;
    return s.slice(0, n) + " …[+" + (s.length - n) + " chars]";
  }

  // describe() walks a value and returns a structure-only clone: string
  // VALUES longer than MAX_VAL are replaced with a "<str:N chars>" marker plus
  // a short sample, arrays are summarized by length + first element shape.
  function describe(val, depth) {
    depth = depth || 0;
    if (depth > 8) return "<max-depth>";
    if (val === null) return null;
    var t = typeof val;
    if (t === "string") {
      if (val.length <= MAX_VAL) return val;
      return (
        "<str:" +
        val.length +
        " chars> sample=" +
        JSON.stringify(val.slice(0, MAX_VAL)) +
        " …[truncated, redact before sharing]"
      );
    }
    if (t === "number" || t === "boolean") return val;
    if (t === "undefined") return "<undefined>";
    if (Array.isArray(val)) {
      var arr = { __array_len: val.length };
      if (val.length > 0) arr.__first_elem = describe(val[0], depth + 1);
      if (val.length > 1)
        arr.__last_elem = describe(val[val.length - 1], depth + 1);
      return arr;
    }
    if (t === "object") {
      var o = {};
      for (var k in val) {
        if (Object.prototype.hasOwnProperty.call(val, k)) {
          try {
            o[k] = describe(val[k], depth + 1);
          } catch (e) {
            o[k] = "<describe-error: " + String(e) + ">";
          }
        }
      }
      return o;
    }
    return "<" + t + ">";
  }

  // findKeys() walks an object and returns every path whose key matches re.
  // Used to auto-locate token/usage fields wherever the site buries them.
  function findKeys(val, re, path, out, depth) {
    out = out || [];
    path = path || "";
    depth = depth || 0;
    if (depth > 8 || val === null || typeof val !== "object") return out;
    if (Array.isArray(val)) {
      for (var idx = 0; idx < val.length && idx < 50; idx++) {
        findKeys(val[idx], re, path + "[" + idx + "]", out, depth + 1);
      }
      return out;
    }
    for (var k in val) {
      if (!Object.prototype.hasOwnProperty.call(val, k)) continue;
      var childPath = path ? path + "." + k : k;
      var v = val[k];
      if (re.test(k) && (typeof v === "number" || typeof v === "string")) {
        out.push({ path: childPath, value: v });
      }
      if (v && typeof v === "object") {
        findKeys(v, re, childPath, out, depth + 1);
      }
    }
    return out;
  }

  function matchesCaptureURL(url) {
    try {
      var u = new URL(url, location.origin);
      if (SITE.capturePaths) return SITE.capturePaths.indexOf(u.pathname) !== -1;
      if (SITE.capturePathRe) return SITE.capturePathRe.test(u.pathname);
      if (SITE.capturePathContains) {
        var needles = Array.isArray(SITE.capturePathContains)
          ? SITE.capturePathContains
          : [SITE.capturePathContains];
        for (var n = 0; n < needles.length; n++) {
          if (u.pathname.indexOf(needles[n]) !== -1) return true;
        }
        return false;
      }
      return false;
    } catch (e) {
      return false;
    }
  }

  function newCapture(url, method, bodyText) {
    var cap = {
      idx: STATE.captures.length,
      site: SITE.site,
      transport: SITE.transport,
      request_url: url,
      method: method || "GET",
      request_body_raw: typeof bodyText === "string" ? bodyText : "",
      request_body_struct: null,
      request_body_parse_error: null,
      frames: [], // truncated samples for the dump (up to MAX_FRAMES)
      frame_count: 0,
      raw_buffer: "", // full raw stream (capped) for __sboRaw() / extraction
      parsed_frames: [], // parsed SSE data objects, for extraction
      flags: {},
      started_at: new Date().toISOString(),
      ended_at: null,
      latency_ms: 0,
      _perf0: (window.performance && performance.now()) || 0,
    };
    // Describe request body structure.
    if (typeof bodyText === "string" && bodyText.length) {
      try {
        cap.request_body_struct = describe(JSON.parse(bodyText), 0);
      } catch (e) {
        cap.request_body_parse_error = String(e);
        cap.request_body_struct =
          "<non-JSON body, len=" +
          bodyText.length +
          "> sample=" +
          JSON.stringify(bodyText.slice(0, MAX_VAL));
      }
    }
    STATE.captures.push(cap);
    return cap;
  }

  function recordFrameSample(cap, raw) {
    cap.frame_count++;
    if (cap.frames.length < MAX_FRAMES) {
      cap.frames.push(truncStr(raw, MAX_FRAME_CHARS));
    }
  }

  function appendRaw(cap, chunk) {
    if (cap.raw_buffer.length < MAX_RAW_BYTES) {
      cap.raw_buffer += chunk;
    } else if (!cap.flags.raw_truncated) {
      cap.flags.raw_truncated = true;
    }
  }

  // feedSSE splits an incoming decoded chunk into SSE frames, records a
  // truncated sample of each, and tries to JSON.parse each `data:` payload,
  // pushing parsed objects into cap.parsed_frames. Fail-soft per frame.
  function feedSSE(cap, chunk) {
    appendRaw(cap, chunk);
    var lines = chunk.split("\n");
    for (var li = 0; li < lines.length; li++) {
      var line = lines[li];
      var t = line.replace(/\r$/, "").trim();
      if (t === "") continue;
      recordFrameSample(cap, t);
      // Track event: lines (Claude uses named events).
      if (t.indexOf("event:") === 0) {
        var evName = t.slice(6).trim();
        cap.flags.event_types = cap.flags.event_types || {};
        cap.flags.event_types[evName] =
          (cap.flags.event_types[evName] || 0) + 1;
        continue;
      }
      if (t.indexOf("data:") !== 0) continue;
      var payload = t.slice(5).trim();
      if (payload === "" || payload === "[DONE]") continue;
      // ChatGPT resume/second-stream canary.
      if (
        payload.indexOf("stream_handoff") !== -1 ||
        payload.indexOf("resume_") !== -1
      ) {
        cap.flags.possible_second_stream = true;
      }
      try {
        var obj = JSON.parse(payload);
        if (cap.parsed_frames.length < 5000) cap.parsed_frames.push(obj);
      } catch (e) {
        cap.flags.parse_errors = (cap.flags.parse_errors || 0) + 1;
      }
    }
  }

  // feedRaw is the batchexecute (Gemini) path: no per-line SSE framing; just
  // buffer everything and sample the head.
  function feedRaw(cap, chunk) {
    appendRaw(cap, chunk);
    if (cap.frames.length < MAX_FRAMES) {
      recordFrameSample(cap, chunk);
    } else {
      cap.frame_count++;
    }
  }

  function endCapture(cap) {
    if (cap.ended_at) return;
    cap.ended_at = new Date().toISOString();
    if (window.performance && cap._perf0) {
      cap.latency_ms = Math.max(0, Math.round(performance.now() - cap._perf0));
    }
    extract(cap);
    console.log(
      "%c[SBO] ✓ turn captured (" +
        SITE.site +
        ", capture #" +
        cap.idx +
        "). Run __sboDump() to print, __sboStop() to disarm.",
      "color:#0a0;font-weight:bold"
    );
    dumpOne(cap);
  }

  // --- per-site best-effort extraction (the PATHS are the deliverable) -----
  function extract(cap) {
    var ex = {
      model: "",
      model_path: "",
      prompt_text_sample: "",
      prompt_path: "",
      response_text_sample: "",
      response_path: "",
      conversation_id: "",
      conversation_id_path: "",
      message_id: "",
      message_id_path: "",
      tokens: [],
    };
    try {
      if (SITE.site === "chatgpt-web") extractChatGPT(cap, ex);
      else if (SITE.site === "claude-web") extractClaude(cap, ex);
      else if (SITE.site === "perplexity-web") extractPerplexity(cap, ex);
      else if (SITE.site === "gemini-web") extractGemini(cap, ex);
      else if (SITE.site === "copilot-web") extractCopilot(cap, ex);
    } catch (e) {
      ex.extract_error = String(e);
    }
    // Generic token/usage field sweep across all parsed frames.
    try {
      var re = /token|usage|prompt_tokens|completion_tokens/i;
      for (var f = 0; f < cap.parsed_frames.length; f++) {
        var hits = findKeys(cap.parsed_frames[f], re, "data", []);
        for (var h = 0; h < hits.length; h++) {
          ex.tokens.push({
            field: hits[h].path,
            value: hits[h].value,
            path: hits[h].path,
          });
          if (ex.tokens.length >= 20) break;
        }
        if (ex.tokens.length >= 20) break;
      }
    } catch (e2) {
      /* ignore */
    }
    cap.extracted = ex;
  }

  function extractRequestJSON(cap) {
    try {
      return JSON.parse(cap.request_body_raw);
    } catch (e) {
      return null;
    }
  }

  function extractChatGPT(cap, ex) {
    var j = extractRequestJSON(cap);
    if (j) {
      if (j.model) {
        ex.model = j.model;
        ex.model_path = "request.model";
      }
      if (j.conversation_id) {
        ex.conversation_id = j.conversation_id;
        ex.conversation_id_path = "request.conversation_id";
      }
      var msgs = Array.isArray(j.messages) ? j.messages : [];
      for (var m = msgs.length - 1; m >= 0; m--) {
        var mm = msgs[m];
        if (mm && mm.author && mm.author.role === "user") {
          var parts = (mm.content && mm.content.parts) || [];
          ex.prompt_text_sample = truncStr(
            parts.filter(function (p) {
              return typeof p === "string";
            }).join(""),
            MAX_VAL
          );
          ex.prompt_path =
            "request.messages[" + m + "].content.parts[]  (last author.role=user)";
          break;
        }
      }
    }
    // Response: snapshot message.content.parts supersedes; v-string deltas.
    var best = "";
    for (var f = 0; f < cap.parsed_frames.length; f++) {
      var ev = cap.parsed_frames[f];
      var msg = ev.message || (ev.v && ev.v.message) || null;
      if (ev.conversation_id && !ex.conversation_id) {
        ex.conversation_id = ev.conversation_id;
        ex.conversation_id_path = "data.conversation_id";
      }
      if (msg) {
        if (msg.id) {
          ex.message_id = msg.id;
          ex.message_id_path = "data.message.id";
        }
        if (msg.metadata && msg.metadata.model_slug) {
          ex.model = msg.metadata.model_slug;
          ex.model_path = "data.message.metadata.model_slug";
        }
        var joined = ((msg.content && msg.content.parts) || [])
          .filter(function (p) {
            return typeof p === "string";
          })
          .join("");
        if (joined.length > best.length) {
          best = joined;
          ex.response_path = "data.message.content.parts[]  (snapshot)";
        }
      } else if (typeof ev.v === "string") {
        best += ev.v;
        if (!ex.response_path) ex.response_path = "data.v  (append delta)";
      }
    }
    ex.response_text_sample = truncStr(best, MAX_VAL);
  }

  function extractClaude(cap, ex) {
    var j = extractRequestJSON(cap);
    if (j) {
      if (j.model) {
        ex.model = j.model;
        ex.model_path = "request.model";
      }
      if (typeof j.prompt === "string") {
        ex.prompt_text_sample = truncStr(j.prompt, MAX_VAL);
        ex.prompt_path = "request.prompt";
      } else if (Array.isArray(j.messages) && j.messages.length) {
        ex.prompt_path = "request.messages[last].content[]";
      }
    }
    // conversation id from URL path.
    try {
      var u = new URL(cap.request_url, location.origin);
      var mu = u.pathname.match(/\/chat_conversations\/([^/]+)\//);
      if (mu) {
        ex.conversation_id = mu[1];
        ex.conversation_id_path = "request_url path /chat_conversations/{id}/";
      }
    } catch (e) {
      /* ignore */
    }
    var text = "";
    for (var f = 0; f < cap.parsed_frames.length; f++) {
      var ev = cap.parsed_frames[f];
      if (ev.type === "content_block_delta" && ev.delta) {
        if (typeof ev.delta.text === "string") {
          text += ev.delta.text;
          ex.response_path = "data.delta.text  (type=content_block_delta)";
        }
      } else if (ev.type === "message_start" && ev.message) {
        if (ev.message.id) {
          ex.message_id = ev.message.id;
          ex.message_id_path = "data.message.id  (type=message_start)";
        }
        if (ev.message.model) {
          ex.model = ev.message.model;
          ex.model_path = "data.message.model  (type=message_start)";
        }
      } else if (typeof ev.completion === "string") {
        text += ev.completion;
        if (!ex.response_path) ex.response_path = "data.completion  (legacy)";
      }
    }
    ex.response_text_sample = truncStr(text, MAX_VAL);
  }

  function extractPerplexity(cap, ex) {
    var j = extractRequestJSON(cap);
    if (j) {
      if (typeof j.query_str === "string") {
        ex.prompt_text_sample = truncStr(j.query_str, MAX_VAL);
        ex.prompt_path = "request.query_str";
      } else if (typeof j.q === "string") {
        ex.prompt_text_sample = truncStr(j.q, MAX_VAL);
        ex.prompt_path = "request.q";
      }
      if (j.model_preference || j.model) {
        ex.model = j.model_preference || j.model;
        ex.model_path = j.model_preference
          ? "request.model_preference"
          : "request.model";
      }
      if (j.frontend_context_uuid || j.context_uuid) {
        ex.conversation_id = j.frontend_context_uuid || j.context_uuid;
        ex.conversation_id_path = j.frontend_context_uuid
          ? "request.frontend_context_uuid"
          : "request.context_uuid";
      }
    }
    var best = "";
    for (var f = 0; f < cap.parsed_frames.length; f++) {
      var ev = cap.parsed_frames[f];
      var answer =
        (typeof ev.answer === "string" && ev.answer) ||
        (typeof ev.text === "string" && ev.text) ||
        "";
      if (answer.length > best.length) {
        best = answer;
        ex.response_path =
          typeof ev.answer === "string" ? "data.answer" : "data.text";
      }
      if (ev.backend_uuid && !ex.conversation_id) {
        ex.conversation_id = ev.backend_uuid;
        ex.conversation_id_path = "data.backend_uuid";
      }
      if (ev.uuid && !ex.message_id) {
        ex.message_id = ev.uuid;
        ex.message_id_path = "data.uuid";
      }
    }
    ex.response_text_sample = truncStr(best, MAX_VAL);
    ex.note =
      "Perplexity answer field name varies (answer / text / chunks) — verify the path against the frame samples.";
  }

  function extractGemini(cap, ex) {
    // BatchExecute: )]}' prefix + nested JSON-array envelope. BEST-EFFORT:
    // pull the longest decodable JSON string literal from the raw buffer.
    ex.note =
      "Gemini uses BatchExecute RPC — extraction is BEST-EFFORT. Run __sboRaw(" +
      cap.idx +
      ") and paste the FULL (redacted) envelope so we can find the exact text path.";
    var buf = cap.raw_buffer;
    if (buf.indexOf(")]}'") === 0) {
      ex.flags_batchexecute_prefix = "envelope starts with )]}' as expected";
    }
    try {
      var matches = buf.match(/"((?:[^"\\]|\\.){40,})"/g) || [];
      var best = "";
      for (var mi = 0; mi < matches.length; mi++) {
        try {
          var s = JSON.parse(matches[mi]);
          if (typeof s === "string" && s.length > best.length) best = s;
        } catch (e) {
          /* skip */
        }
      }
      ex.response_text_sample = truncStr(best, MAX_VAL);
      ex.response_path =
        "<best-effort longest-string heuristic — NO stable JSON path yet; needs raw envelope>";
    } catch (e2) {
      ex.extract_error = String(e2);
    }
    // Prompt: the batchexecute f.req form-encoded param carries it; note only.
    ex.prompt_path =
      "<request is form-encoded f.req= — prompt is nested in the RPC arg array; needs raw request body>";
  }

  function extractCopilot(cap, ex) {
    ex.note =
      "Copilot uses a WebSocket transport — see the ws_frames captured on this record, not SSE frames.";
  }

  // --- fetch interception (SSE + batchexecute) -----------------------------
  STATE.originals.fetch = window.fetch;
  window.fetch = function (input, init) {
    var url = typeof input === "string" ? input : (input && input.url) || "";
    var method = (init && init.method) || (input && input.method) || "GET";
    var matched = matchesCaptureURL(url);
    // Log EVERY fetch (matched or not, same-origin or cross-origin) so the
    // conduit host is visible via __sboUrls(). URL + method only — no body.
    logURL("fetch", method, url, matched);
    if (!matched) {
      return STATE.originals.fetch.apply(this, arguments);
    }
    var bodyText = "";
    try {
      if (init && typeof init.body === "string") bodyText = init.body;
    } catch (e) {
      /* ignore */
    }
    var cap = newCapture(url, method, bodyText);
    console.log(
      "%c[SBO] ← capture request: " + method + " " + url,
      "color:#06c"
    );

    var p = STATE.originals.fetch.apply(this, arguments);
    return p.then(function (resp) {
      try {
        if (!resp.body) {
          endCapture(cap);
          return resp;
        }
        var tee = resp.body.tee();
        var a = tee[0];
        var b = tee[1];
        (function () {
          var reader = a.getReader();
          var dec = new TextDecoder();
          function pump() {
            return reader.read().then(function (r) {
              if (r.done) {
                endCapture(cap);
                return;
              }
              try {
                var chunk = dec.decode(r.value, { stream: true });
                if (SITE.transport === "batchexecute") feedRaw(cap, chunk);
                else feedSSE(cap, chunk);
              } catch (e) {
                cap.flags.frame_decode_error = String(e);
              }
              return pump();
            });
          }
          pump().catch(function (e) {
            cap.flags.stream_error = String(e);
            endCapture(cap);
          });
        })();
        return new Response(b, {
          status: resp.status,
          statusText: resp.statusText,
          headers: resp.headers,
        });
      } catch (e) {
        cap.flags.tap_error = String(e);
        endCapture(cap);
        return resp;
      }
    });
  };

  // --- XMLHttpRequest interception (fallback transport) --------------------
  STATE.originals.xhrOpen = XMLHttpRequest.prototype.open;
  STATE.originals.xhrSend = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.open = function (method, url) {
    try {
      this.__sboMethod = method;
      this.__sboURL = url;
      // URL-discovery log (URL + method only) — a cross-origin conduit could
      // ride an XHR rather than fetch; surface it in __sboUrls() too.
      logURL("xhr", method, url, matchesCaptureURL(url));
    } catch (e) {
      /* ignore */
    }
    return STATE.originals.xhrOpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.send = function (body) {
    var self = this;
    try {
      if (this.__sboURL && matchesCaptureURL(this.__sboURL)) {
        var cap = newCapture(
          this.__sboURL,
          this.__sboMethod,
          typeof body === "string" ? body : ""
        );
        console.log(
          "%c[SBO] ← capture request (XHR): " +
            cap.method +
            " " +
            cap.request_url,
          "color:#06c"
        );
        var lastLen = 0;
        this.addEventListener("progress", function () {
          try {
            var rt = self.responseText || "";
            if (rt.length > lastLen) {
              var delta = rt.slice(lastLen);
              lastLen = rt.length;
              if (SITE.transport === "batchexecute") feedRaw(cap, delta);
              else feedSSE(cap, delta);
            }
          } catch (e) {
            /* ignore */
          }
        });
        this.addEventListener("loadend", function () {
          try {
            var rt = self.responseText || "";
            if (rt.length > lastLen) {
              var delta = rt.slice(lastLen);
              if (SITE.transport === "batchexecute") feedRaw(cap, delta);
              else feedSSE(cap, delta);
            }
          } catch (e) {
            /* ignore */
          }
          endCapture(cap);
        });
      }
    } catch (e) {
      /* fail-soft */
    }
    return STATE.originals.xhrSend.apply(this, arguments);
  };

  // --- WebSocket interception (Copilot + ChatGPT conduit) ------------------
  // Copilot streams its whole completion over a WS to copilot.microsoft.com.
  // ChatGPT thinking/conduit models stream the VISIBLE completion over a
  // SECOND-leg WebSocket to an UNKNOWN host (not the SSE /f/conversation
  // response) — so for chatgpt-web we arm on ALL WS connections and buffer the
  // raw frame stream for reverse-engineering. Arming is a capability decision
  // (site == chatgpt-web OR copilot-web), never a single hardcoded host.
  STATE.originals.WebSocket = window.WebSocket;
  function SBOWebSocket(url, protocols) {
    var ws =
      protocols === undefined
        ? new STATE.originals.WebSocket(url)
        : new STATE.originals.WebSocket(url, protocols);
    var arm = false;
    try {
      if (SITE.site === "copilot-web") {
        arm = /copilot\.microsoft\.com/.test(String(url));
      } else if (SITE.site === "chatgpt-web") {
        // Conduit host is unknown ahead of time — capture ALL WS the page opens.
        arm = true;
      }
    } catch (e) {
      /* ignore */
    }
    // Log EVERY WebSocket the page opens (armed or not) so __sboUrls() reveals
    // the real conduit host/path even before we know it.
    logURL("ws", "WS", url, arm);
    if (!arm) return ws;

    var cap = newCapture(String(url), "WS", "");
    // Standalone WS capture record. For chatgpt-web the site transport is "sse"
    // (the SSE leg); this record is the websocket leg, so tag it explicitly.
    cap.transport = "websocket";
    cap.ws_frames = { sent: [], received: [] };
    console.log("%c[SBO] ← capture WebSocket: " + url, "color:#06c");

    var origSend = ws.send.bind(ws);
    ws.send = function (data) {
      try {
        if (typeof data === "string" && cap.ws_frames.sent.length < 20) {
          cap.ws_frames.sent.push(truncStr(data, MAX_FRAME_CHARS));
          try {
            var jj = JSON.parse(data);
            var frame = Array.isArray(jj) ? jj[0] : jj;
            if (frame && (frame.event === "send" || frame.type === "send")) {
              cap.extracted_ws_prompt_path =
                "ws.send frame.content|text|message  (event=send)";
            }
          } catch (e) {
            /* control frame */
          }
        }
      } catch (e) {
        /* ignore */
      }
      return origSend(data);
    };

    ws.addEventListener("message", function (evt) {
      try {
        if (typeof evt.data !== "string") {
          // Binary frame — record a size placeholder only, never buffer raw
          // binary. (Copilot uses text frames, so this path is chatgpt-only in
          // practice.)
          var nbytes = "?";
          try {
            if (evt.data && typeof evt.data.byteLength === "number")
              nbytes = evt.data.byteLength;
            else if (evt.data && typeof evt.data.size === "number")
              nbytes = evt.data.size;
          } catch (eb) {
            /* ignore */
          }
          if (cap.ws_frames.received.length < MAX_FRAMES) {
            cap.ws_frames.received.push("<binary " + nbytes + " bytes>");
          }
          cap.flags.ws_binary_frames = (cap.flags.ws_binary_frames || 0) + 1;
          return;
        }
        if (cap.ws_frames.received.length < MAX_FRAMES) {
          cap.ws_frames.received.push(truncStr(evt.data, MAX_FRAME_CHARS));
        }
        if (SITE.site === "chatgpt-web") {
          // Unknown conduit frame shape — accumulate the FULL concatenated text
          // payload (bounded by MAX_RAW_BYTES via appendRaw) so __sboRaw(idx)
          // can dump the whole conduit stream for reverse-engineering. No
          // structured parse and no auto-end (no known "done" event yet).
          appendRaw(cap, evt.data);
          cap.frame_count++;
          cap.ws_response_path =
            "<chatgpt conduit WS — unknown frame shape; see raw_buffer via __sboRaw(" +
            cap.idx +
            ")>";
          return;
        }
        // copilot-web: structured JSON frames (appendText / done). Unchanged.
        var jj = JSON.parse(evt.data);
        var frames = Array.isArray(jj) ? jj : [jj];
        for (var fi = 0; fi < frames.length; fi++) {
          var f = frames[fi];
          var ev = f && (f.event || f.type);
          if (ev === "appendText" && typeof f.text === "string") {
            cap.raw_buffer += f.text;
            cap.ws_response_path = "ws frame.text  (event=appendText)";
          } else if (ev === "done") {
            cap.extracted = cap.extracted || {};
            endCapture(cap);
          }
        }
      } catch (e) {
        /* malformed frame */
      }
    });
    return ws;
  }
  SBOWebSocket.prototype = STATE.originals.WebSocket.prototype;
  SBOWebSocket.CONNECTING = STATE.originals.WebSocket.CONNECTING;
  SBOWebSocket.OPEN = STATE.originals.WebSocket.OPEN;
  SBOWebSocket.CLOSING = STATE.originals.WebSocket.CLOSING;
  SBOWebSocket.CLOSED = STATE.originals.WebSocket.CLOSED;
  window.WebSocket = SBOWebSocket;

  // --- dump / stop / raw ---------------------------------------------------
  function dumpOne(cap) {
    // Build a CapturedTurn-aligned view + the diagnostic detail.
    var view = {
      "// PRIVACY":
        "This contains SAMPLES of YOUR prompt/response — redact the *_sample / *_raw / *_body fields before sharing. The parser only needs the STRUCTURE and field PATHS.",
      capture_index: cap.idx,
      site: cap.site,
      transport: cap.transport,
      request_url: cap.request_url,
      method: cap.method,
      latency_ms: cap.latency_ms,
      frame_count: cap.frame_count,
      flags: cap.flags,
      request_body_key_structure: cap.request_body_struct,
      first_frame_samples: cap.frames,
      ws_frames: cap.ws_frames || undefined,
      best_effort_extraction: cap.extracted,
      captured_turn_mapping: {
        schema_version: 1,
        site: cap.site,
        conversation_id:
          (cap.extracted && cap.extracted.conversation_id) || "",
        conversation_id_path:
          (cap.extracted && cap.extracted.conversation_id_path) || "",
        message_id: (cap.extracted && cap.extracted.message_id) || "",
        message_id_path: (cap.extracted && cap.extracted.message_id_path) || "",
        model: (cap.extracted && cap.extracted.model) || "",
        model_path: (cap.extracted && cap.extracted.model_path) || "",
        prompt_text_sample: (cap.extracted && cap.extracted.prompt_text_sample) || "",
        prompt_text_path: (cap.extracted && cap.extracted.prompt_path) || "",
        response_text_sample:
          (cap.extracted && cap.extracted.response_text_sample) || "",
        response_text_path: (cap.extracted && cap.extracted.response_path) || "",
        token_fields_found: (cap.extracted && cap.extracted.tokens) || [],
      },
    };
    console.log(
      "%c===== SBO CAPTURE #" + cap.idx + " (" + cap.site + ") =====",
      "color:#a06;font-weight:bold"
    );
    console.log(JSON.stringify(view, null, 2));
    return view;
  }

  window.__sboDump = function () {
    if (STATE.captures.length === 0) {
      console.warn(
        "[SBO] No turns captured yet. Send ONE message on this page, then run __sboDump()."
      );
      return;
    }
    console.log(
      "%c[SBO] Dumping " +
        STATE.captures.length +
        " capture(s). REDACT the *_sample / *_raw / *_body fields before sharing.",
      "color:#a60;font-weight:bold"
    );
    var all = [];
    for (var d = 0; d < STATE.captures.length; d++) {
      all.push(dumpOne(STATE.captures[d]));
    }
    return all;
  };

  window.__sboRaw = function (idx) {
    idx = idx || 0;
    var cap = STATE.captures[idx];
    if (!cap) {
      console.warn("[SBO] No capture at index " + idx + ".");
      return;
    }
    console.log(
      "%c[SBO] FULL raw response buffer for capture #" +
        idx +
        " — REDACT before sharing. Useful for Gemini (paste the whole envelope).",
      "color:#a60;font-weight:bold"
    );
    console.log(cap.raw_buffer);
    return cap.raw_buffer;
  };

  window.__sboUrls = function () {
    console.log(
      "%c[SBO] URL log — " +
        STATE.urlLog.length +
        " entr" +
        (STATE.urlLog.length === 1 ? "y" : "ies") +
        (STATE.urlLogTruncated ? " (capped at " + MAX_URL_LOG + ")" : "") +
        ". URLs + methods ONLY (no bodies/headers). ✓ = matched a capture rule. " +
        "On a ChatGPT thinking turn, look for the conduit ws:// host here.",
      "color:#a60;font-weight:bold"
    );
    for (var i = 0; i < STATE.urlLog.length; i++) {
      var e = STATE.urlLog[i];
      console.log(
        "#" +
          e.seq +
          "  " +
          (e.matched ? "✓" : " ") +
          "  [" +
          e.kind +
          "]  " +
          e.method +
          "  " +
          e.url
      );
    }
    return STATE.urlLog;
  };

  window.__sboStop = function () {
    try {
      if (STATE.originals.fetch) window.fetch = STATE.originals.fetch;
      if (STATE.originals.xhrOpen)
        XMLHttpRequest.prototype.open = STATE.originals.xhrOpen;
      if (STATE.originals.xhrSend)
        XMLHttpRequest.prototype.send = STATE.originals.xhrSend;
      if (STATE.originals.WebSocket)
        window.WebSocket = STATE.originals.WebSocket;
    } catch (e) {
      /* ignore */
    }
    STATE.__armed = false;
    console.log(
      "%c[SBO] Disarmed — original fetch/XHR/WebSocket restored. Captured data is still in window.__SBO.captures (run __sboDump()).",
      "color:#666"
    );
  };

  // --- armed banner --------------------------------------------------------
  console.log(
    "%c▶ SBO capture armed — site=" +
      SITE.site +
      " (" +
      SITE.transport +
      "). Send ONE message now.",
    "background:#111;color:#0f0;font-size:14px;padding:6px 10px;font-weight:bold"
  );
  console.log(
    "%c[SBO] Passive & console-only: NO network calls are made by this harness. " +
      "On turn end it auto-prints; or run __sboDump(). __sboRaw(i) prints full raw. " +
      "__sboUrls() lists every fetch/WS/XHR URL (conduit-host discovery). " +
      "__sboStop() restores originals. REDACT text samples before sharing.",
    "color:#888"
  );
})();
