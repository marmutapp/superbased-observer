// content-main.js — MAIN-world capture interceptor for the SuperBased
// Observer browser extension.
//
// Runs in the page's own JS realm (world: MAIN), so it can wrap the page's
// fetch / XMLHttpRequest / WebSocket and read request bodies + streamed
// response bodies as they arrive — the only MV3 surface that can read a
// streamed response without CDP (proposal §2.1).
//
// SITE = DATA. Every per-site difference (host, capture endpoints, transport,
// how to pull the prompt/response out of the wire) is a ROW in the SITES
// table below; the interception plumbing is site-agnostic. Adding a site is a
// new row, not a new code path — except WebSocket vs SSE, which are genuinely
// different transports (a capability shape, not a name branch), so they get
// two interceptors that both feed the same emitTurn().
//
// Honesty: Gemini uses Google's BatchExecute RPC — a fragmented wrapper far
// harder to parse than SSE. Its parser here is BEST-EFFORT / incomplete: it
// records whatever it can extract (often just the fact a turn happened +
// estimated tokens) rather than crashing. This is marked in the row and in
// the site's Gap in the Go registry.

(() => {
  "use strict";

  const NONCE =
    (crypto && crypto.randomUUID && crypto.randomUUID()) ||
    String(Math.random()).slice(2);
  const MSG_TYPE = "SBO_BROWSER_TURN";
  const HEALTH_TYPE = "SBO_BROWSER_HEALTH";

  // --- per-site parsers (pure module, loaded before this script) -----------
  // The request parsers + streaming accumulators live in src/parsers.js (the
  // SBOParsers UMD global, injected before this file per the manifest
  // content_scripts ordering), so the SAME parsing code is unit-tested under
  // Node (src/parsers.test.js). This IIFE owns only the transport patching +
  // the isolated-world bridge; the per-site parsing is DATA.
  const P =
    (typeof globalThis !== "undefined" && globalThis.SBOParsers) || null;
  if (!P) {
    // parsers.js failed to load before us — fail-soft: do nothing rather than
    // throw into the page. Manifest ordering normally guarantees it is present.
    return;
  }
  const {
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
  } = P;

  // --- SITES table ---------------------------------------------------------
  // hostMatch: substring test on location.hostname.
  // isCaptureURL / makeAccumulator / parseRequest are the per-site hooks.
  const SITES = [
    {
      site: "chatgpt-web",
      transport: "sse",
      hostMatch: "chatgpt.com",
      // A PATH PREDICATE, not a static allow-list: match the two conversation
      // bases AND their `/resume` second leg (a thinking turn's answer streams
      // there — LIVE-CONFIRMED 2026-07-11), but never the `/prepare` keystroke
      // debounce (a partial-query, NOT a prompt).
      capturePathFn(pathname) {
        if (pathname.endsWith("/prepare")) return false;
        return (
          pathname === "/backend-api/conversation" ||
          pathname === "/backend-api/f/conversation" ||
          pathname === "/backend-api/conversation/resume" ||
          pathname === "/backend-api/f/conversation/resume"
        );
      },
      parseRequest: parseChatGPTRequest,
      makeAccumulator: makeChatGPTAccumulator,
      // Handoff correlation capability (§ two-leg thinking turn). The transport
      // layer owns the pending-turn map + flush timer; the SITE supplies the
      // pure per-site predicates (site = data, not a code branch). Absent on
      // every other site → those emit one turn per stream, unchanged.
      correlateHandoff: {
        isSecondLeg: (pathname) => isChatGPTResumePath(pathname),
        shouldBuffer: (state) => chatGPTIsHandoffPending(state),
        merge: (leg1, state) => mergeChatGPTLegs(leg1, state),
      },
    },
    {
      site: "claude-web",
      transport: "sse",
      hostMatch: "claude.ai",
      // /api/organizations/{org}/chat_conversations/{conv}/completion
      capturePathRe: /\/chat_conversations\/[^/]+\/completion$/,
      parseRequest: parseClaudeRequest,
      makeAccumulator: makeClaudeAccumulator,
      // Pull the conversation id out of the URL path.
      conversationIdFromURL(u) {
        const m = u.pathname.match(/\/chat_conversations\/([^/]+)\//);
        return m ? m[1] : "";
      },
    },
    {
      site: "perplexity-web",
      transport: "sse",
      hostMatch: "perplexity.ai",
      capturePaths: ["/rest/sse/perplexity_ask"],
      parseRequest: parsePerplexityRequest,
      makeAccumulator: makePerplexityAccumulator,
    },
    {
      site: "gemini-web",
      transport: "sse",
      hostMatch: "gemini.google.com",
      // The CHAT call is a StreamGenerate RPC (no rpcid on the chat call);
      // batchexecute is kept as a fallback segment for older builds.
      capturePathContainsAny: ["StreamGenerate", "/BardChatUi/data/batchexecute"],
      parseRequest: parseGeminiRequest,
      makeAccumulator: makeGeminiAccumulator,
      bestEffort: true,
      // TRANSPORT CAPABILITIES (LIVE-CONFIRMED 2026-07-11):
      // - xhrStreaming: StreamGenerate is issued as an XMLHttpRequest, not
      //   fetch, so the response is captured from the XHR body (see the XHR
      //   capture tap below), else Gemini records an empty response.
      // - streamUntyped: the batchexecute response is NOT `text/event-stream`
      //   typed, so the fetch-path stream gate can't rely on content-type.
      xhrStreaming: true,
      streamUntyped: true,
    },
    {
      site: "copilot-web",
      transport: "websocket",
      hostMatch: "copilot.microsoft.com",
    },
  ];

  const SITE = SITES.find((s) => location.hostname.includes(s.hostMatch));
  if (!SITE) return; // page matched a manifest origin we don't parse — no-op.

  function isCaptureURL(url) {
    try {
      const u = new URL(url, location.origin);
      if (u.origin !== location.origin) return false;
      if (SITE.capturePathFn) return !!SITE.capturePathFn(u.pathname);
      if (SITE.capturePaths) return SITE.capturePaths.includes(u.pathname);
      if (SITE.capturePathRe) return SITE.capturePathRe.test(u.pathname);
      if (SITE.capturePathContainsAny)
        return SITE.capturePathContainsAny.some((seg) =>
          u.pathname.includes(seg)
        );
      if (SITE.capturePathContains)
        return u.pathname.includes(SITE.capturePathContains);
      return false;
    } catch {
      return false;
    }
  }

  // Per-site health: count parse successes + shape canaries so the daemon /
  // dashboard learn about endpoint churn from telemetry, not user reports.
  const health = { site: SITE.site, captures: 0, empties: 0, lastAt: 0 };
  function pingHealth() {
    try {
      window.postMessage(
        { __sbo: HEALTH_TYPE, nonce: NONCE, health: { ...health } },
        location.origin
      );
    } catch {
      /* ignore */
    }
  }

  function emitTurn(reqInfo, acc, startedAt) {
    if (acc.finalize) {
      try {
        acc.finalize();
      } catch {
        /* best effort */
      }
    }
    const s = acc.state;
    const conversationId =
      s.conversationId || reqInfo.conversationId || "";
    const hasSomething = conversationId || s.text || reqInfo.prompt;
    health.lastAt = Date.now();
    if (!hasSomething) {
      health.empties++;
      pingHealth();
      return; // nothing usable — a shape canary tripped
    }
    health.captures++;
    const turn = {
      site: SITE.site,
      conversation_id: conversationId,
      message_id: s.messageId || "",
      model: s.model || reqInfo.model || "",
      request_url: reqInfo.url,
      prompt_text: reqInfo.prompt || "",
      response_text: s.text || "",
      latency_ms: Math.max(0, Math.round(performance.now() - startedAt)),
      captured_at: new Date().toISOString(),
      best_effort: !!SITE.bestEffort,
    };
    // Real usage counts when the site's stream carried them (Claude web —
    // TODO(must-verify-live): the consumer endpoint may omit usage). Additive:
    // when absent the isolated world / server fall back to a chars/4 estimate.
    if (s.inputTokens) turn.prompt_tokens_est = s.inputTokens;
    if (s.outputTokens) turn.response_tokens_est = s.outputTokens;
    window.postMessage({ __sbo: MSG_TYPE, nonce: NONCE, turn }, location.origin);
    pingHealth();
  }

  function safePath(url) {
    try {
      return new URL(url, location.origin).pathname;
    } catch {
      return "";
    }
  }

  // --- two-leg handoff correlation (transport lifecycle) -------------------
  // A site with a `correlateHandoff` capability (ChatGPT) splits a thinking
  // turn across two SSE POSTs: leg 1 carries the prompt but hands off; leg 2
  // (the `/resume` stream) carries the answer. This map buffers leg 1 keyed by
  // conversation id until the resume leg completes; a flush timer guarantees a
  // buffered turn is emitted even if the resume leg never arrives, so nothing
  // leaks or hangs. Every other site has no `correlateHandoff` and never
  // touches this path — it emits one turn per stream, unchanged.
  const handoffPending = new Map();
  const HANDOFF_FLUSH_MS = 120000;

  // completeSSETurn is the single finalize→emit chokepoint for the fetch AND
  // XHR streaming paths. It finalizes the accumulator, then (for a
  // correlateHandoff site) decides buffer vs merge vs direct-emit from the
  // FINALIZED state — never from a hostname branch.
  function completeSSETurn(reqInfo, acc, startedAt) {
    if (acc.finalize) {
      try {
        acc.finalize();
      } catch {
        /* best effort */
      }
    }
    const s = acc.state;
    const corr = SITE.correlateHandoff;
    if (corr) {
      const path = safePath(reqInfo.url);
      const convId = s.conversationId || reqInfo.conversationId || "";
      if (corr.isSecondLeg(path)) {
        // Leg 2 (resume): the answer stream. Merge with the buffered leg-1
        // prompt when present; otherwise emit what the resume leg carried.
        const pending = convId ? handoffPending.get(convId) : null;
        if (pending) {
          clearTimeout(pending.timer);
          handoffPending.delete(convId);
          const merged = corr.merge(pending.leg1, s);
          emitTurn(
            {
              url: pending.reqInfo.url,
              prompt: merged.prompt,
              model: merged.model,
              conversationId: merged.conversationId,
            },
            {
              state: {
                text: merged.text,
                conversationId: merged.conversationId,
                messageId: merged.messageId,
                model: merged.model,
              },
            },
            pending.startedAt
          );
          return;
        }
        emitTurn(reqInfo, acc, startedAt); // unmatched resume — emit as-is
        return;
      }
      if (convId && corr.shouldBuffer(s)) {
        // Leg 1 of a thinking turn: buffer the prompt until the resume leg.
        const prev = handoffPending.get(convId);
        if (prev) {
          // A new prompt in the same conversation before the prior resumed —
          // flush the earlier leg-1 (don't silently drop its prompt).
          clearTimeout(prev.timer);
          emitTurn(prev.reqInfo, prev.acc, prev.startedAt);
          handoffPending.delete(convId);
        }
        const entry = {
          reqInfo,
          acc,
          leg1: {
            prompt: reqInfo.prompt,
            model: reqInfo.model,
            conversationId: convId,
          },
          startedAt,
        };
        entry.timer = setTimeout(() => {
          handoffPending.delete(convId);
          emitTurn(entry.reqInfo, entry.acc, entry.startedAt);
        }, HANDOFF_FLUSH_MS);
        handoffPending.set(convId, entry);
        return;
      }
    }
    emitTurn(reqInfo, acc, startedAt);
  }

  // --- pre-send intervention (proposal §7) --------------------------------
  // BEST-EFFORT local guardrail, NOT airtight DLP. It sits between the
  // composer JS and the real network call, in the SAME JS realm — so the MV3
  // no-blocking-webRequest restriction (a network-layer rule) does not apply.
  // This is DISTINCT from the Plane-B internal/guard egress policy (that
  // governs the developer's own coding-agent tool calls, a different
  // enforcement point); do not conflate them.
  //
  // HONEST MV3 LIMITS (documented in README "Pre-send intervention"):
  //   * We can only be as strong as the transports we patch — fetch / XHR /
  //     WebSocket.send / sendBeacon. A NEW transport, or page code that grabs
  //     a transport reference BEFORE our document_start patch lands, silently
  //     defeats it.
  //   * `warn` is ADVISORY (shows an overlay, does not hold the request);
  //     `redact` rewrites the outbound body; `block` drops the call.
  //   * declarativeNetRequest could add a coarse URL/host backstop but cannot
  //     inspect a body — not built here (documented note only).
  //   * The composer DOM submit/keydown is a SECONDARY signal only, never a
  //     sufficient hard block on its own.
  const Tier1 =
    (typeof globalThis !== "undefined" && globalThis.SBOTier1) || null;

  // Intervention config is READ in the isolated world (chrome.storage) and
  // relayed DOWN here via SBO_CONFIG. Default `off` → the document_start race
  // window before it arrives is harmless. Nonce-guarded like every bridge msg.
  let interventionCfg = { mode: "off", types: [] };
  window.addEventListener("message", (event) => {
    if (event.source !== window || event.origin !== location.origin) return;
    const d = event.data;
    if (!d || d.__sbo !== "SBO_CONFIG" || d.nonce !== NONCE) return;
    if (d.intervention && typeof d.intervention.mode === "string") {
      interventionCfg = {
        mode: d.intervention.mode,
        types: Array.isArray(d.intervention.types) ? d.intervention.types : [],
      };
    }
  });

  function filterByType(spans) {
    if (!interventionCfg.types || interventionCfg.types.length === 0) {
      return spans;
    }
    const allow = new Set(interventionCfg.types);
    return spans.filter((s) => allow.has(s.type));
  }

  function summarizeSpans(spans) {
    const counts = {};
    for (const s of spans) counts[s.type] = (counts[s.type] || 0) + 1;
    return Object.keys(counts)
      .map((t) => t + " ×" + counts[t])
      .join(", ");
  }

  // In-page overlay. Trusted-Types-safe: createElement + textContent only (no
  // innerHTML / script sink), so it works even under copilot.microsoft.com's
  // require-trusted-types-for 'script' CSP.
  let overlayEl = null;
  function showOverlay(kind, detail) {
    try {
      if (!overlayEl) {
        overlayEl = document.createElement("div");
        overlayEl.setAttribute("data-sbo-overlay", "1");
        const s = overlayEl.style;
        s.position = "fixed";
        s.zIndex = "2147483647";
        s.bottom = "16px";
        s.right = "16px";
        s.maxWidth = "360px";
        s.padding = "12px 14px";
        s.borderRadius = "8px";
        s.font = "13px/1.4 system-ui, sans-serif";
        s.color = "#fff";
        s.boxShadow = "0 4px 16px rgba(0,0,0,.35)";
        (document.body || document.documentElement).appendChild(overlayEl);
      }
      overlayEl.style.background = kind === "block" ? "#8a1c1c" : "#7a5a00";
      const title =
        kind === "block"
          ? "SuperBased Observer blocked this send"
          : "SuperBased Observer — sensitive data detected";
      overlayEl.textContent = title + ": " + detail;
      overlayEl.style.display = "block";
      clearTimeout(showOverlay._t);
      showOverlay._t = setTimeout(() => {
        if (overlayEl) overlayEl.style.display = "none";
      }, 6000);
    } catch {
      /* overlay is cosmetic — never let it break a send */
    }
  }

  // applyIntervention scans one outgoing body string. Returns
  // { block, body }: body is the (possibly redacted) string to actually send.
  // Fail-open: any error → proceed unchanged (best-effort, never breaks a
  // send). Only called for capture-URL sends (the prompt), not telemetry.
  function applyIntervention(bodyText) {
    const mode = interventionCfg.mode;
    if (mode === "off" || !Tier1 || typeof bodyText !== "string" || !bodyText) {
      return { block: false, body: bodyText };
    }
    let spans;
    try {
      spans = filterByType(Tier1.detect(bodyText));
    } catch {
      return { block: false, body: bodyText };
    }
    if (!spans || spans.length === 0) return { block: false, body: bodyText };
    if (mode === "redact") {
      try {
        return { block: false, body: Tier1.applyRedaction(bodyText, spans) };
      } catch {
        return { block: false, body: bodyText };
      }
    }
    if (mode === "block") {
      showOverlay("block", summarizeSpans(spans));
      return { block: true, body: bodyText };
    }
    // warn (default enforcing mode): advisory overlay, proceed.
    showOverlay("warn", summarizeSpans(spans));
    return { block: false, body: bodyText };
  }

  // --- universal transport patches for intervention ------------------------
  // XHR.send + sendBeacon are not part of the capture path, but a complete
  // guardrail must patch EVERY known transport (§7.2). These sites use
  // fetch/WS today; patching XHR + sendBeacon closes the "new transport"
  // gap for the composer prompt. Capture-URL scoped so we never touch
  // unrelated requests.
  (function patchXHR() {
    const OrigOpen = XMLHttpRequest.prototype.open;
    const OrigSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.open = function (method, url) {
      try {
        this.__sboURL = url;
      } catch {
        /* ignore */
      }
      return OrigOpen.apply(this, arguments);
    };
    XMLHttpRequest.prototype.send = function (body) {
      let sendBody = body;
      try {
        if (
          typeof body === "string" &&
          this.__sboURL &&
          isCaptureURL(this.__sboURL)
        ) {
          const res = applyIntervention(body);
          if (res.block) {
            // Hard stop: abort before the bytes leave. The page sees a failed
            // request (best-effort block; documented it can disrupt UX).
            try {
              this.abort();
            } catch {
              /* ignore */
            }
            return;
          }
          if (res.body !== body) sendBody = res.body;

          // CAPTURE tap for XHR-transport SSE-family sites (Gemini's
          // StreamGenerate is issued as an XHR, not fetch — LIVE-CONFIRMED
          // 2026-07-11). Without this the response never reaches an
          // accumulator and Gemini records an empty answer. The Gemini
          // accumulator buffers the whole body and harvests on finalize, so
          // feeding the full responseText once at load is correct; error/abort
          // finish defensively so a pending turn never hangs. Capability-gated
          // (SITE.xhrStreaming), so it never touches unrelated XHRs.
          if (SITE.xhrStreaming && SITE.makeAccumulator) {
            try {
              const reqBody = typeof sendBody === "string" ? sendBody : "";
              const parsed = SITE.parseRequest ? SITE.parseRequest(reqBody) : {};
              const reqInfo = { url: this.__sboURL, ...parsed };
              if (!reqInfo.conversationId && SITE.conversationIdFromURL) {
                try {
                  reqInfo.conversationId = SITE.conversationIdFromURL(
                    new URL(this.__sboURL, location.origin)
                  );
                } catch {
                  /* ignore */
                }
              }
              const acc = SITE.makeAccumulator();
              const startedAt = performance.now();
              let done = false;
              const xhr = this;
              const finish = () => {
                if (done) return;
                done = true;
                try {
                  const rt =
                    typeof xhr.responseText === "string" ? xhr.responseText : "";
                  if (rt) acc.feed(rt);
                } catch {
                  /* responseType may block responseText — feed nothing */
                }
                completeSSETurn(reqInfo, acc, startedAt);
              };
              this.addEventListener("load", finish);
              this.addEventListener("error", finish);
              this.addEventListener("abort", finish);
            } catch {
              /* fail-soft: no capture, request unaffected */
            }
          }
        }
      } catch {
        /* fail-open */
      }
      if (sendBody !== body) return OrigSend.call(this, sendBody);
      return OrigSend.apply(this, arguments);
    };
  })();

  (function patchBeacon() {
    if (!navigator.sendBeacon) return;
    const OrigBeacon = navigator.sendBeacon.bind(navigator);
    navigator.sendBeacon = function (url, data) {
      try {
        if (typeof data === "string" && isCaptureURL(url)) {
          const res = applyIntervention(data);
          if (res.block) return false; // report failure to the caller
          if (res.body !== data) return OrigBeacon(url, res.body);
        }
      } catch {
        /* fail-open */
      }
      return OrigBeacon(url, data);
    };
  })();

  // Non-SSE sites (e.g. the WebSocket Copilot site) don't fold intervention
  // into a capture fetch wrapper, so patch fetch here too — every transport
  // covered. No-op unless the site declares capture URLs.
  if (SITE.transport !== "sse") {
    const origFetchI = window.fetch;
    window.fetch = async function (input, init) {
      const url =
        typeof input === "string" ? input : (input && input.url) || "";
      if (
        isCaptureURL(url) &&
        init &&
        typeof init.body === "string" &&
        interventionCfg.mode !== "off"
      ) {
        const res = applyIntervention(init.body);
        if (res.block) {
          throw new DOMException(
            "SuperBased Observer blocked this request (sensitive data detected).",
            "AbortError"
          );
        }
        if (res.body !== init.body) {
          const newInit = Object.assign({}, init, { body: res.body });
          return origFetchI.call(this, input, newInit);
        }
      }
      return origFetchI.apply(this, arguments);
    };
  }

  // --- SSE interception via fetch ------------------------------------------
  if (SITE.transport === "sse") {
    const origFetch = window.fetch;
    window.fetch = async function (input, init) {
      const url =
        typeof input === "string" ? input : (input && input.url) || "";
      if (!isCaptureURL(url)) return origFetch.apply(this, arguments);

      let reqBody = "";
      try {
        if (init && typeof init.body === "string") reqBody = init.body;
      } catch {
        /* ignore */
      }

      // Pre-send intervention on the outgoing prompt body, BEFORE origFetch.
      // block → reject the fetch (hard stop); redact → send the rewritten
      // body (and capture the redacted prompt, since that is what actually
      // reached the model). Only string init.body is rewritable (a Request-
      // object body is not — documented limitation).
      let fetchArgs = arguments;
      if (reqBody) {
        const res = applyIntervention(reqBody);
        if (res.block) {
          throw new DOMException(
            "SuperBased Observer blocked this request (sensitive data detected).",
            "AbortError"
          );
        }
        if (res.body !== reqBody) {
          reqBody = res.body;
          const newInit = Object.assign({}, init, { body: res.body });
          fetchArgs = [input, newInit];
        }
      }

      const parsed = SITE.parseRequest(reqBody);
      const reqInfo = { url, ...parsed };
      if (!reqInfo.conversationId && SITE.conversationIdFromURL) {
        try {
          reqInfo.conversationId = SITE.conversationIdFromURL(
            new URL(url, location.origin)
          );
        } catch {
          /* ignore */
        }
      }
      const startedAt = performance.now();

      const resp = await origFetch.apply(this, fetchArgs);
      try {
        const ct = resp.headers.get("content-type") || "";
        const streamed =
          ct.includes("text/event-stream") ||
          !!SITE.streamUntyped; // batchexecute isn't event-stream typed
        if (streamed && resp.body) {
          const acc = SITE.makeAccumulator();
          const [a, b] = resp.body.tee();
          (async () => {
            try {
              const reader = a.getReader();
              const dec = new TextDecoder();
              for (;;) {
                const { value, done } = await reader.read();
                if (done) break;
                acc.feed(dec.decode(value, { stream: true }));
              }
              completeSSETurn(reqInfo, acc, startedAt);
            } catch {
              completeSSETurn(reqInfo, acc, startedAt); // fail-soft
            }
          })();
          return new Response(b, {
            status: resp.status,
            statusText: resp.statusText,
            headers: resp.headers,
          });
        }
      } catch {
        /* fail-soft: untouched response */
      }
      return resp;
    };
  }

  // --- WebSocket interception (Copilot) ------------------------------------
  // copilot.microsoft.com streams over a WebSocket: the client sends a `send`
  // frame with the prompt; the server streams `appendText` frames and a
  // terminal `done`. cf_clearance-gated, but we only OBSERVE the already-open
  // authenticated socket. A distinct transport from SSE — the capability
  // shape differs, so this is its own interceptor (not a table branch).
  if (SITE.transport === "websocket") {
    const OrigWS = window.WebSocket;
    function SBOWebSocket(url, protocols) {
      const ws =
        protocols === undefined
          ? new OrigWS(url)
          : new OrigWS(url, protocols);
      // Only tap the Copilot chat socket (skip telemetry/other sockets).
      let isChat = false;
      try {
        isChat = /copilot\.microsoft\.com/.test(String(url));
      } catch {
        /* ignore */
      }
      if (!isChat) return ws;

      const acc = { text: "", conversationId: "", messageId: "", model: "" };
      let prompt = "";
      let startedAt = performance.now();

      const origSend = ws.send.bind(ws);
      ws.send = function (data) {
        try {
          if (typeof data === "string") {
            const j = JSON.parse(data);
            const frame = Array.isArray(j) ? j[0] : j;
            if (frame && (frame.event === "send" || frame.type === "send")) {
              const rawPrompt =
                frame.content || frame.text || frame.message || "";
              // Pre-send intervention on the WebSocket `send` frame — the
              // JS-call-layer intercept applies to individual WS messages,
              // which declarativeNetRequest cannot touch (§7.2).
              const res = applyIntervention(rawPrompt);
              if (res.block) {
                acc.text = "";
                prompt = "";
                return; // hard stop: never emit the frame
              }
              if (res.body !== rawPrompt) {
                // Rewrite the redacted prompt back into whichever field the
                // frame used, re-stringify, and send THAT.
                if (frame.content !== undefined) frame.content = res.body;
                else if (frame.text !== undefined) frame.text = res.body;
                else if (frame.message !== undefined) frame.message = res.body;
                const rebuilt = JSON.stringify(Array.isArray(j) ? [frame] : frame);
                prompt = res.body;
                startedAt = performance.now();
                acc.text = "";
                if (frame.conversationId)
                  acc.conversationId = frame.conversationId;
                return origSend(rebuilt);
              }
              prompt = rawPrompt;
              startedAt = performance.now();
              acc.text = "";
              if (frame.conversationId) acc.conversationId = frame.conversationId;
            }
          }
        } catch {
          /* non-JSON control frame — ignore */
        }
        return origSend(data);
      };

      ws.addEventListener("message", (evt) => {
        try {
          if (typeof evt.data !== "string") return;
          const j = JSON.parse(evt.data);
          const frames = Array.isArray(j) ? j : [j];
          for (const f of frames) {
            const ev = f && (f.event || f.type);
            if (ev === "appendText" && typeof f.text === "string") {
              acc.text += f.text;
            } else if (ev === "setOptions") {
              if (f.model) acc.model = f.model;
              if (f.conversationId) acc.conversationId = f.conversationId;
            } else if (ev === "done") {
              emitTurn(
                { url: String(url), prompt, model: acc.model, conversationId: acc.conversationId },
                { state: acc },
                startedAt
              );
            }
          }
        } catch {
          /* ignore malformed frame */
        }
      });
      return ws;
    }
    SBOWebSocket.prototype = OrigWS.prototype;
    SBOWebSocket.CONNECTING = OrigWS.CONNECTING;
    SBOWebSocket.OPEN = OrigWS.OPEN;
    SBOWebSocket.CLOSING = OrigWS.CLOSING;
    SBOWebSocket.CLOSED = OrigWS.CLOSED;
    window.WebSocket = SBOWebSocket;
  }

  // Handshake: expose the nonce to the isolated world.
  window.postMessage({ __sbo: "SBO_HELLO", nonce: NONCE, site: SITE.site }, location.origin);
})();
