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

  // genCaptureId mints an OPAQUE random per-turn id (MED-1 + MED-3). It is the
  // daemon's id-less fallback session/message key — a tier BELOW conversation_id
  // and message_id — and REPLACES the old Go-side content-hash fallback, which
  // was both privacy-leaky (a pre-scrub content hash riding into exported ids)
  // and collision-prone (at usage_only granularity the content is stripped, so
  // distinct id-less turns hashed identically). This id is NOT derived from any
  // content and is NOT the conversation id: it is pure randomness, generated
  // once per emitted turn. crypto.randomUUID is present in every MV3 content-
  // script realm; the getRandomValues hex path is a belt-and-braces fallback.
  function genCaptureId() {
    try {
      if (typeof crypto !== "undefined" && crypto.randomUUID) {
        return crypto.randomUUID();
      }
    } catch {
      /* fall through to the byte path */
    }
    try {
      const b = new Uint8Array(16);
      crypto.getRandomValues(b);
      let hex = "";
      for (let i = 0; i < b.length; i++) {
        hex += b[i].toString(16).padStart(2, "0");
      }
      return hex;
    } catch {
      // Last-resort (crypto entirely absent) — still opaque, still per-turn.
      return "cap_" + Date.now().toString(16) + Math.random().toString(16).slice(2);
    }
  }

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
    makeChatGPTWSAccumulator,
    chatGPTWSSplitFrame,
    makeClaudeAccumulator,
    makePerplexityAccumulator,
    makePerplexityThreadResolver,
    makeGeminiAccumulator,
    parseCopilotSendFrame,
    copilotTurnDisposition,
    copilotShouldEmit,
    copilotTaintNext,
    makeCopilotAccumulator,
    copilotJoinContent,
    rewriteCopilotContent,
    capContentField,
    resolveIdSource,
    hasResponseContent,
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
      // POST bases (+ the logged-out `/backend-anon/*` set), but never the
      // `/prepare` keystroke debounce (a partial-query, NOT a prompt).
      //
      // TWO TRANSPORTS, ONE TURN (chatgpt-web) — GROUNDED 2026-07-17 by live
      // logged-in CDP recon: the `/resume` second-leg entries were REMOVED.
      // A thinking turn's ANSWER NO LONGER streams over a `/resume` POST — the
      // leg-1 POST `/f/conversation` returns a PURE `stream_handoff` (no answer
      // text) and the answer streams over the GLOBAL `ws.chatgpt.com` WebSocket
      // (see `wsAnswer` below). This fetch/SSE tap runs pre-send prompt
      // redaction (applyIntervention) AND still emits IF the SSE leg actually
      // carried an answer (a simple non-thinking turn could answer here) — the
      // WS emit and the SSE emit dedup each other by turn id, so neither
      // double-counts.
      capturePathFn(pathname) {
        if (pathname.endsWith("/prepare")) return false;
        return (
          pathname === "/backend-api/conversation" ||
          pathname === "/backend-api/f/conversation" ||
          // Logged-out / anonymous endpoint set. GROUNDED (moderate confidence)
          // on the canonical chat2api / anonymous-chatgpt reverse-engineering
          // projects: unauthenticated ChatGPT posts to `/backend-anon/...`. The
          // fetch tap redacts the outbound prompt + captures an SSE-side answer.
          pathname === "/backend-anon/conversation" ||
          pathname === "/backend-anon/f/conversation"
        );
      },
      parseRequest: parseChatGPTRequest,
      // The SSE leg accumulator (used only when a turn answers on SSE — see the
      // hasResponseContent gate in completeSSETurn). A thinking turn's leg-1 is
      // a contentless handoff, so this typically harvests nothing and the WS tap
      // emits instead.
      makeAccumulator: makeChatGPTAccumulator,
      // requireStreamComplete gates completeSSETurn on acc.state.complete: the
      // SSE leg emits ONLY when the answer reached a recognized end-of-turn
      // signal (re-review HIGH #1). A stream that errored/closed after
      // harvesting only partial text must NOT emit — a truncated emit would
      // land in recentEmits and SUPPRESS the WS leg's complete answer. A
      // fully-streamed inline turn (free-tier) carries the terminal patch +
      // message_stream_complete, so it still emits.
      requireStreamComplete: true,
      // wsAnswer is an ADDITIVE capability (site = data, not a code branch):
      // the WebSocket interceptor fires for ANY site declaring it and taps the
      // matching host's socket with a Map of pure per-turn accumulators.
      // Distinct from copilot's EXCLUSIVE `transport:"websocket"` (which OWNS
      // the whole transport). Here transport stays "sse" so the fetch-based
      // pre-send redaction intervention keeps working; the WS carries the answer
      // + prompt echo + conversation_id + turn_id + model, so it is WS-PRIMARY.
      wsAnswer: {
        // The answer socket host (`ws.chatgpt.com`) is DIFFERENT from the page
        // host (`chatgpt.com`), but `new WebSocket()` runs in the chatgpt.com
        // MAIN-world realm, so our window.WebSocket patch sees it. Tap by EXACT
        // hostname equality (not substring — `ws.chatgpt.com.evil` must miss).
        hostMatch: "ws.chatgpt.com",
        makeAccumulator: makeChatGPTWSAccumulator,
        // PER-ELEMENT splitter for the multiplexed global socket: returns one
        // routing part per frame element `{ turnStart, topicId (== turn_id),
        // completeConvId, frame }`, so a frame BATCHING two turns is fanned out
        // to each turn's own accumulator (never appended onto the first topic).
        split: chatGPTWSSplitFrame,
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
      // ADDITIVE capability (site = data, not a code branch): Perplexity's
      // response backend_uuid is PER-ASK, not per-thread, so the conversation
      // id must be RESOLVED (from the /search/<id> URL, then the
      // last_backend_uuid chain) at turn-finalization. A SITE that declares
      // threadResolverFactory gets one per-page resolver instance whose result
      // overrides the emitted conversation_id + id_source. See
      // parsers.js::makePerplexityThreadResolver.
      threadResolverFactory: makePerplexityThreadResolver,
      // ADDITIVE capability (site = data, not a code branch): Perplexity issues
      // its ask via a PRISTINE same-origin about:blank iframe's
      // contentWindow.fetch, grabbed SYNCHRONOUSLY by top-frame code
      // (LIVE-CONFIRMED 2026-07-18) — so the top-frame window.fetch patch never
      // sees it. Capture requires BOTH the all_frames manifest injection (layer
      // a — our fetch patch lands inside the iframe realm) AND the top-frame
      // contentWindow-getter race backstop (layer b — wraps the iframe realm's
      // fetch the instant it is grabbed). This flag gates the layer-(b) install
      // + the cross-layer emit dedup below.
      iframeFetchGrab: true,
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

  // resolveHost returns the effective hostname for SITE selection. In a normal
  // top frame this is location.hostname. In a same-origin about:blank iframe
  // (Perplexity's pristine-iframe ask realm — LIVE-CONFIRMED 2026-07-18)
  // location.hostname is "" even though the SECURITY ORIGIN is inherited from
  // the parent, so fall back to window.top's hostname (same-origin → readable)
  // then document.baseURI. Every access is try-guarded (a cross-origin
  // window.top read throws; we then have no business here anyway).
  function resolveHost() {
    try {
      if (location.hostname) return location.hostname;
    } catch {
      /* ignore */
    }
    try {
      if (
        window.top &&
        window.top !== window &&
        window.top.location &&
        window.top.location.hostname
      ) {
        return window.top.location.hostname;
      }
    } catch {
      /* cross-origin top — not our same-origin iframe case */
    }
    try {
      const u = new URL(document.baseURI);
      if (u.hostname) return u.hostname;
    } catch {
      /* ignore */
    }
    return "";
  }

  // coerceBodyString returns a string form of a fetch/XHR request body when
  // possible. Gemini's StreamGenerate + Perplexity's ask may send the body as a
  // URLSearchParams (not a raw string) with the prompt encoded inside, so coerce
  // it for the request parser. Only string + URLSearchParams are coerced;
  // FormData / Blob / ArrayBuffer bodies yield "" (no prompt harvest, request
  // untouched). Used ONLY for CAPTURE (prompt parsing), never to rewrite a
  // non-string body (which would corrupt the request encoding).
  function coerceBodyString(body) {
    if (typeof body === "string") return body;
    try {
      if (
        typeof URLSearchParams !== "undefined" &&
        body instanceof URLSearchParams
      ) {
        return body.toString();
      }
    } catch {
      /* ignore */
    }
    return "";
  }

  const HOST = resolveHost();
  const SITE = SITES.find((s) => HOST.includes(s.hostMatch));
  if (!SITE) return; // page matched a manifest origin we don't parse — no-op.

  // Per-page conversation-id resolver for sites whose response id is per-turn,
  // not per-thread (Perplexity). The resolver holds the last_backend_uuid →
  // thread chain map; resolve() is consulted at turn finalization
  // (completeSSETurn) to override the emitted conversation_id.
  //
  // Perplexity capture ALSO runs inside pristine same-origin about:blank iframes
  // (the layer-a realm — see the iframe-fetch backstop below). A PER-REALM
  // resolver would give each iframe its OWN empty chain map + its OWN blank
  // pathname, fragmenting ONE thread across N Observer sessions (adversarial
  // review r2-1 / r3-1). Centralize ONE resolver + ONE chain map in the TOP
  // frame, shared by every same-origin realm via a Symbol.for-keyed singleton on
  // window.top — a well-known cross-realm symbol (the SAME Symbol object in every
  // realm of the process), far less page-discoverable than a string global, and
  // the state it holds is metadata only (the backend_uuid → thread-id chain).
  // Cross-origin / unreadable window.top falls back to a realm-local resolver
  // (status quo). The per-turn backend_uuid iframe-layer dedup (recentEmits,
  // completeSSETurn) is unchanged — it stays per-realm and keys on the
  // turn-unique frontend_uuid + response backend_uuid.
  const SHARED_RESOLVER_KEY =
    typeof Symbol !== "undefined" && Symbol.for
      ? Symbol.for("sbo.pplx.threadResolver")
      : "__sboPplxThreadResolver";
  function getSharedThreadResolver(factory) {
    if (!factory) return null;
    try {
      const top = window.top;
      void top.location.href; // same-origin probe (throws for a cross-origin top)
      if (!top[SHARED_RESOLVER_KEY]) top[SHARED_RESOLVER_KEY] = factory();
      return top[SHARED_RESOLVER_KEY];
    } catch {
      return factory(); // cross-origin / unreadable top → realm-local resolver
    }
  }
  const threadResolver = getSharedThreadResolver(SITE.threadResolverFactory);

  // readThreadPathname reads the EFFECTIVE thread pathname. Because capture also
  // runs inside about:blank iframes (whose OWN location.pathname is blank), read
  // the TOP frame's pathname when same-origin reachable, falling back to this
  // realm's location only when window.top is cross-origin / unreadable
  // (adversarial review r2-1 / r3-1).
  function readThreadPathname() {
    try {
      const top = window.top;
      if (top && top.location && typeof top.location.pathname === "string") {
        return top.location.pathname;
      }
    } catch {
      /* cross-origin / unreadable top — fall through to this realm */
    }
    try {
      return location.pathname || "";
    } catch {
      return "";
    }
  }

  // safeExtractURL pulls the request URL out of a fetch first-arg (string /
  // Request `.url` / URL `.href` / stringifiable) with EACH attempt guarded
  // INDEPENDENTLY (adversarial review r2-4): a branded Request/URL-like object
  // whose `url` getter THROWS must not disable the `.href` + String() fallbacks
  // (a single wrapping try would swallow the getter throw, yield "", and
  // silently bypass capture). Returns "" only when every attempt fails.
  function safeExtractURL(input) {
    if (typeof input === "string") return input;
    if (input == null) return "";
    let url = "";
    try {
      if (typeof input.url === "string") url = input.url;
    } catch {
      /* throwing url getter — try the next route */
    }
    if (!url) {
      try {
        if (typeof input.href === "string") url = input.href;
      } catch {
        /* throwing href getter — try String() */
      }
    }
    if (!url) {
      try {
        url = String(input);
      } catch {
        url = "";
      }
    }
    return url;
  }

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
  // id_missing counts captures that emitted with NO conversation id — the
  // daemon distinguishes "capturing but uncorrelated" from a stale parser.
  const health = {
    site: SITE.site,
    captures: 0,
    empties: 0,
    id_missing: 0,
    lastAt: 0,
  };
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

  // --- idle heartbeat (endpoint-churn / logged-out visibility) -------------
  // A matched site that fires ZERO capture-URL wire events since page load is
  // otherwise INVISIBLE to the daemon (no beacon at all — endpoint churn, or a
  // logged-out session hitting an unmatched endpoint set). Emit a
  // status:"idle" health beacon after IDLE_MS of silence so the gap is legible
  // in `observer browser health`. Content scripts (unlike the MV3 service
  // worker) live for the page lifetime, so a plain interval suffices; the
  // relay path (postMessage → isolated → SW → native host) already exists.
  const IDLE_MS = 3 * 60 * 1000; // page-load-relative silence window (default 3m)
  let wireEvents = 0;
  let idleTimer = null;
  function noteWireEvent() {
    wireEvents++;
    if (idleTimer) {
      clearInterval(idleTimer);
      idleTimer = null;
    }
  }
  function pingIdle() {
    try {
      window.postMessage(
        { __sbo: HEALTH_TYPE, nonce: NONCE, health: { ...health, status: "idle" } },
        location.origin
      );
    } catch {
      /* ignore */
    }
  }
  idleTimer = setInterval(() => {
    // Once any capture-URL wire event lands, the normal capture beacons take
    // over and the heartbeat stops.
    if (wireEvents > 0) {
      clearInterval(idleTimer);
      idleTimer = null;
      return;
    }
    pingIdle();
  }, IDLE_MS);

  // emitTurn finalizes + emits one turn. opts.idSource overrides the computed
  // id provenance (used by the two-leg merge, which tags "resume", and by the
  // Perplexity thread resolver, which tags "url"/"chain"). opts.conversationId
  // overrides the emitted conversation id — used by the Perplexity resolver to
  // ship the stable THREAD id instead of the per-turn response backend_uuid,
  // WITHOUT mutating acc.state.conversationId (still the turn-unique dedup key).
  function emitTurn(reqInfo, acc, startedAt, opts) {
    if (acc.finalize) {
      try {
        acc.finalize();
      } catch {
        /* best effort */
      }
    }
    const s = acc.state;
    const conversationId =
      (opts && opts.conversationId) ||
      s.conversationId ||
      reqInfo.conversationId ||
      "";
    const hasSomething = conversationId || s.text || reqInfo.prompt;
    health.lastAt = Date.now();
    if (!hasSomething) {
      health.empties++;
      pingHealth();
      return; // nothing usable — a shape canary tripped
    }
    health.captures++;
    // A capture actually COMPLETED — only NOW is this a wire event for idle
    // purposes (LOW-2). A matched request that never reaches here (blocked,
    // rejected, content-type change, resume that never arrives) must leave the
    // idle heartbeat armed so the daemon still learns the tab is capturing-less.
    noteWireEvent();
    // EMIT-ANYWAY posture: a turn with prompt/text but no conversation id
    // still emits (the daemon accepts it via a synthetic session key) — but is
    // tagged so the daemon + health can tell an id-less emit apart.
    const idSource = resolveIdSource(
      s.conversationId,
      reqInfo.conversationId,
      opts && opts.idSource
    );
    if (!conversationId) health.id_missing++;
    const turn = {
      site: SITE.site,
      conversation_id: conversationId,
      // OPAQUE random per-turn id (MED-1 + MED-3): the daemon's id-less fallback
      // session/message key, a tier below conversation_id/message_id. One turn =
      // one capture_id, minted here at the single emit chokepoint, so a turn's
      // tool event and token event (both derived downstream from THIS payload)
      // share it. Not content-derived, not the conversation id.
      capture_id: genCaptureId(),
      id_source: idSource, // additive: "none"|"request"|"stream"|"resume"|"url"|"chain"
      message_id: s.messageId || "",
      model: s.model || reqInfo.model || "",
      // title: harvested from copilot's `titleUpdate` frame (the CapturedTurn
      // wire contract carries it). Additive — empty for sites that never emit one.
      // Routed through the same capContentField chokepoint as prompt/response so
      // a non-string (array-shaped) title is coerced+capped, never emitted raw.
      title: capContentField(s.title || ""),
      request_url: reqInfo.url,
      // Cap each content field well under the daemon's 8 MiB ingest cap
      // (capContentField, pure layer) so one huge paste/answer can't get the
      // WHOLE turn keyed-dropped by the daemon backstop; the marker keeps the
      // truncation legible. The daemon-side keyed drop remains the backstop.
      prompt_text: capContentField(reqInfo.prompt || ""),
      response_text: capContentField(s.text || ""),
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

  // --- cross-transport emit dedup (two transports, one turn) ---------------
  // chatgpt-web can, in principle, capture a turn on BOTH the SSE leg AND the
  // ws.chatgpt.com answer socket. Both emit paths consult this short-TTL,
  // bounded map keyed by a TURN-UNIQUE id: the WS uses state.messageId (the
  // turn_id) and the SSE uses state.handoffId (the stream_handoff
  // turn_exchange_id, which equals the same turn_id). A key is only deduped when
  // present, so distinct SSE-only turns (no handoff id → keyed by nothing) are
  // never suppressed against each other. (Live recon shows ALL answers stream
  // over WS, so this is defense-in-depth against a mixed/older arch.)
  const recentEmits = new Map(); // key -> expiresAt (performance.now ms)
  const EMIT_DEDUP_TTL_MS = 60000;
  const EMIT_DEDUP_MAX = 128;
  // markEmitted / wasRecentlyEmitted accept a single key OR an array of keys.
  // The WS emit marks/checks BOTH turn-scoped ids (topic turn_id + working
  // turn id) because we cannot be sure which one the SSE leg's handoffId holds —
  // a match on EITHER means the same turn already emitted on the other transport.
  function markEmitted(keys) {
    const now = performance.now();
    const list = Array.isArray(keys) ? keys : [keys];
    for (const key of list) {
      if (key) recentEmits.set(key, now + EMIT_DEDUP_TTL_MS);
    }
    for (const [k, exp] of recentEmits) {
      if (exp <= now) recentEmits.delete(k);
    }
    while (recentEmits.size > EMIT_DEDUP_MAX) {
      recentEmits.delete(recentEmits.keys().next().value);
    }
  }
  function wasRecentlyEmitted(keys) {
    const now = performance.now();
    const list = Array.isArray(keys) ? keys : [keys];
    for (const key of list) {
      if (!key) continue;
      const exp = recentEmits.get(key);
      if (exp === undefined) continue;
      if (exp <= now) {
        recentEmits.delete(key);
        continue;
      }
      return true;
    }
    return false;
  }

  // --- SSE completion chokepoint -------------------------------------------
  // completeSSETurn is the single finalize→emit chokepoint for the fetch AND
  // XHR streaming paths (claude-web, perplexity-web, gemini-web + the chatgpt
  // SSE leg). It finalizes the accumulator and emits one turn per stream.
  //
  // NOTE (chatgpt-web, 2026-07-17): the obsolete two-leg `correlateHandoff`
  // buffer/merge machinery was REMOVED. For a `wsAnswer` site the answer usually
  // streams over the `ws.chatgpt.com` WebSocket (the wsAnswer tap emits it), so
  // the SSE leg emits ONLY when it actually harvested response content (a simple
  // turn that answered on SSE) — and dedups against the WS emit by turn id so a
  // turn captured on both transports counts once. The pure two-leg helpers
  // remain in parsers.js (+ their unit tests) but are no longer wired.
  function completeSSETurn(reqInfo, acc, startedAt) {
    if (acc.finalize) {
      try {
        acc.finalize();
      } catch {
        /* best effort */
      }
    }
    // Semantic-completion gate (re-review HIGH #1): a site that streams a
    // terminal end-of-turn signal must not emit a truncated/errored partial —
    // it would suppress the WS leg's complete answer via recentEmits. Applies
    // on BOTH the clean-EOF and reader-error call sites (both route here).
    if (SITE.requireStreamComplete && !acc.state.complete) return;
    if (SITE.wsAnswer) {
      // Emit from the SSE leg ONLY when it carried a real answer (leg-1 handoff
      // has none — the WS tap owns that emit). Dedup by turn id against the WS.
      if (!hasResponseContent(acc.state)) return;
      const key = acc.state.handoffId || "";
      if (key && wasRecentlyEmitted(key)) return;
      markEmitted(key);
    }
    if (SITE.iframeFetchGrab) {
      // Cross-layer emit dedup (iframe-realm layer a + the top-frame layer-b
      // backstop could each capture the SAME ask if both wrappers ever fired in
      // one realm). Key on the turn-unique frontend_uuid (request) AND
      // backend_uuid (response) so one ask emits exactly once. (The per-realm
      // __sboFetchPatched flag already guarantees a single wrapper per realm;
      // this is the within-realm belt-and-braces.)
      const keys = [reqInfo.frontendUuid, acc.state.conversationId];
      if (wasRecentlyEmitted(keys)) return;
      markEmitted(keys);
    }
    if (threadResolver) {
      // Per-turn response id → stable THREAD id. The resolver reads the
      // /search/<id> URL here (the impure location read — pure chain logic lives
      // in parsers.js) and falls back to the last_backend_uuid chain map. Its
      // result OVERRIDES the emitted conversation_id + id_source. We do NOT
      // mutate acc.state.conversationId — that per-turn backend_uuid is the
      // iframe-layer dedup key above (and stays turn-unique). pathname reads the
      // TOP-frame URL (readThreadPathname) so an about:blank iframe realm still
      // sees /search/<id>; startPathname is the request-time snapshot (r3-2).
      const res = threadResolver.resolve({
        pathname: readThreadPathname(),
        startPathname: reqInfo.startPathname || "",
        lastBackendUuid: reqInfo.lastBackendUuid,
        ownBackendUuid: acc.state.conversationId,
        requestConversationId: reqInfo.conversationId,
        frontendUuid: reqInfo.frontendUuid,
      });
      emitTurn(reqInfo, acc, startedAt, {
        conversationId: res.conversationId,
        idSource: res.idSource,
      });
      return;
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
          // idle-cancel moved to the completed-capture path (LOW-2): a matched
          // XHR that fails/aborts before finish must leave the heartbeat armed.
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
          // feeding the full responseText once at load is correct.
          // Capability-gated (SITE.xhrStreaming), so it never touches unrelated
          // XHRs.
          if (SITE.xhrStreaming && SITE.makeAccumulator) {
            try {
              const reqBody = coerceBodyString(sendBody);
              const parsed = SITE.parseRequest ? SITE.parseRequest(reqBody) : {};
              // `let` (not const) so the error/abort cleanup can release these
              // closures for GC and neuter a stale `finish` on XHR-object reuse.
              let reqInfo = { url: this.__sboURL, ...parsed };
              if (!reqInfo.conversationId && SITE.conversationIdFromURL) {
                try {
                  reqInfo.conversationId = SITE.conversationIdFromURL(
                    new URL(this.__sboURL, location.origin)
                  );
                } catch {
                  /* ignore */
                }
              }
              let acc = SITE.makeAccumulator();
              const startedAt = performance.now();
              let done = false;
              const xhr = this;
              // detach removes every listener this tap attached to the XHR so a
              // reused (reopen+resend) object never accumulates duplicate
              // listeners. Called on BOTH the successful (`finish`) and failed
              // (`cleanup`) terminal paths — otherwise repeated successful reuse
              // grows the load-listener count and eventually double-emits.
              const detach = () => {
                try {
                  xhr.removeEventListener("load", finish);
                  xhr.removeEventListener("error", cleanup);
                  xhr.removeEventListener("abort", cleanup);
                  xhr.removeEventListener("timeout", cleanup);
                } catch {
                  /* ignore — nothing to remove */
                }
              };
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
                detach(); // symmetric cleanup: no retained listeners on reuse
                acc = null;
                reqInfo = null;
              };
              // LOW-2: capture + emit ONLY on `load` (a successful response).
              // completeSSETurn → emitTurn calls noteWireEvent(), which disarms
              // the idle heartbeat — but reqInfo.prompt alone makes emitTurn's
              // hasSomething true, so wiring `finish` to `error`/`abort` too
              // meant a FAILED Gemini XHR still emitted a turn and cancelled the
              // idle canary. A matched request that fails must instead LEAVE the
              // heartbeat armed (so endpoint churn / blocked requests still
              // surface).
              //
              // LOW-2 residual (XHR-object reuse): `finish` stays attached to
              // `load` after a completed request, and an XMLHttpRequest object
              // can be REOPENED + resent. If the first request errors/aborts,
              // its `load` listener is never invoked but is also never removed;
              // the next send() then adds a SECOND `load` listener, so a later
              // successful load fires TWICE — once through the STALE failed
              // request's closure (its own reqInfo/acc). `cleanup` is therefore
              // attached to the terminal FAILURE events (error/abort/timeout —
              // exactly one loadend fires, and on failure it is never `load`):
              // it is CLEANUP-ONLY. It removes the `load` listener and itself,
              // flips `done` so any residual `finish` is a no-op, and releases
              // acc+reqInfo for GC. It NEVER emits, NEVER calls noteWireEvent /
              // completeSSETurn — so a failed/aborted XHR leaves NO retained
              // load listener and the idle heartbeat stays armed, while a fresh
              // successful send still emits exactly once. (Gemini has no
              // correlateHandoff, so there is no shared pending state to leak; a
              // buffered leg would keep its own HANDOFF_FLUSH_MS timer.)
              const cleanup = () => {
                done = true; // neuter any stale `finish` on this XHR object
                detach();
                acc = null; // release the buffered response body for GC
                reqInfo = null;
              };
              this.addEventListener("load", finish);
              this.addEventListener("error", cleanup);
              this.addEventListener("abort", cleanup);
              this.addEventListener("timeout", cleanup);
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
      // Guarded per-route extraction (r2-4): a throwing `.url` getter must not
      // reject the page's real fetch — safeExtractURL fails soft to "".
      const url = safeExtractURL(input);
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
  // makeCaptureFetch builds the capture wrapper for a fetch realm. It is used
  // for BOTH the top-frame window.fetch (realm = this window) AND — via the
  // Perplexity iframe backstop (layer b) below — an about:blank iframe realm
  // (realm = that iframe's window). `origFetch` is the realm's native fetch (for
  // a foreign realm it MUST be pre-bound to that realm's window, so a bare
  // grabbed-reference call `fetchFn(url)` with `this===undefined` doesn't trip
  // an Illegal-invocation). `realm` supplies the Response constructor so the
  // returned Response belongs to the CALLER's realm (a foreign-realm page that
  // does `instanceof Response` still sees its own Response). Emits always go
  // through THIS content-main's top-of-closure emit chokepoint
  // (completeSSETurn → emitTurn → window.postMessage(location.origin)), which
  // reaches THIS frame's isolated world.
  function makeCaptureFetch(origFetch, realm) {
    const ResponseCtor = (realm && realm.Response) || Response;
    return async function (input, init) {
      // Robust URL extraction: fetch's first arg may be a STRING, a Request
      // (has `.url`), or a URL object (has `.href`, NOT `.url`). LIVE-CONFIRMED
      // 2026-07-18: Perplexity issues its ask as `fetch(new URL(...), init)`, so
      // the old `input.url` read yielded "" and isCaptureURL rejected the ask —
      // the real capture blocker (the request DID reach our wrapper). Each
      // extraction route is guarded independently (safeExtractURL, r2-4) so one
      // throwing getter can't disable the rest.
      const url = safeExtractURL(input);
      if (!isCaptureURL(url)) return origFetch.apply(this, arguments);
      // NOTE: idle-cancel is NOT here (LOW-2). A matched request only counts as
      // a wire event once a capture COMPLETES (emitTurn); a request that is
      // blocked/rejects/never streams must leave the idle heartbeat armed.

      // reqBody = the rewritable STRING body (intervention rewrites only a
      // string); captureBody = the coerced body for prompt PARSING (also covers
      // a URLSearchParams body — Gemini / Perplexity). Never rewrite a
      // non-string body (that would corrupt the request encoding).
      let reqBody = "";
      let captureBody = "";
      try {
        if (init && init.body != null) {
          if (typeof init.body === "string") {
            reqBody = init.body;
            captureBody = init.body;
          } else {
            captureBody = coerceBodyString(init.body);
          }
        }
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
          captureBody = res.body;
          const newInit = Object.assign({}, init, { body: res.body });
          fetchArgs = [input, newInit];
        }
      }

      const parsed = SITE.parseRequest(captureBody);
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
      // Snapshot the thread URL at REQUEST interception (adversarial review
      // r3-2): a slow ask begun in /search/A that completes after an in-SPA nav
      // to /search/B must attribute to A. The resolver prefers this snapshot and
      // only consults the completion-time pathname when the request began
      // OUTSIDE a thread (home / first ask, where the URL appears mid-stream).
      if (SITE.threadResolverFactory) reqInfo.startPathname = readThreadPathname();
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
              // Clean end-of-stream: the response genuinely completed. Emit +
              // disarm idle unconditionally — the successful path is unchanged.
              completeSSETurn(reqInfo, acc, startedAt);
            } catch {
              // Mid-stream error: the response START succeeded (200 + streaming
              // body handed to this reader) but the stream broke before a clean
              // end. That CAN be a real PARTIAL capture — but the idle canary's
              // health must reflect a GENUINE capture, and completeSSETurn →
              // emitTurn → noteWireEvent() would disarm the heartbeat on the
              // strength of the PROMPT alone (emitTurn.hasSomething is loose).
              // So finalize, then branch on real RESPONSE content: if the
              // accumulator harvested assistant text, emit the partial turn (it
              // is legitimate to disarm idle — we DID capture content); if it
              // holds only a prompt / no response text, treat it as a failed
              // request — do NOT emit and do NOT disarm idle, leaving the
              // heartbeat armed so the daemon still learns the tab went quiet.
              try {
                if (acc.finalize) acc.finalize();
              } catch {
                /* best effort — predicate falls through to "no content" */
              }
              if (hasResponseContent(acc.state)) {
                completeSSETurn(reqInfo, acc, startedAt);
              }
              // else: drop — no emit, no noteWireEvent; idle stays armed.
            }
          })();
          return new ResponseCtor(b, {
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

  if (SITE.transport === "sse") {
    // Idempotent per realm (the __sboFetchPatched flag is shared with the
    // Perplexity iframe backstop so a realm is wrapped exactly ONCE — layer a
    // in the iframe realm and layer b's getter never double-wrap the same
    // window).
    if (!window.__sboFetchPatched) {
      window.__sboFetchPatched = true;
      window.fetch = makeCaptureFetch(window.fetch, window);
    }
  }

  // --- Perplexity iframe contentWindow race backstop (layer b) -------------
  // Perplexity issues its ask via a PRISTINE same-origin about:blank iframe's
  // contentWindow.fetch, grabbed SYNCHRONOUSLY by top-frame code before our
  // all_frames content script (layer a) can patch that fresh iframe realm — so
  // layer a LOSES the injection race by construction. Backstop: override the
  // HTMLIFrameElement.prototype.contentWindow getter (top frame only) so that
  // the instant top-frame code READS iframe.contentWindow we wrap that realm's
  // .fetch with our capture wrapper BEFORE returning it — the grabbed reference
  // is therefore ALREADY patched (wins by construction). Idempotent via the
  // shared per-realm __sboFetchPatched flag; NEVER throws (falls through to the
  // native contentWindow on any error); NEVER touches a cross-origin frame.
  // wrapIframeRealm wraps a same-origin child realm's fetch with our capture
  // wrapper exactly once (idempotent via the shared __sboFetchPatched flag).
  // Returns true when it wrapped. Never throws.
  function wrapIframeRealm(win) {
    try {
      if (win && !win.__sboFetchPatched && typeof win.fetch === "function") {
        // Same-origin probe: reading win.location.href throws for a cross-origin
        // frame — only same-origin realms are ours to patch.
        void win.location.href;
        win.__sboFetchPatched = true;
        // Pre-bind the realm's native fetch to win so a bare grabbed call
        // (`this===undefined`) never trips Illegal-invocation.
        win.fetch = makeCaptureFetch(win.fetch.bind(win), win);
        return true;
      }
    } catch {
      /* cross-origin or any error → leave the realm untouched */
    }
    return false;
  }

  // installIframeFetchBackstop wires TWO access-route-agnostic backstops for
  // Perplexity's pristine-about:blank-iframe fetch grab (top frame only):
  //   layer b — override HTMLIFrameElement.prototype.contentWindow so a
  //             property-access grab (`iframe.contentWindow.fetch`) returns an
  //             ALREADY-wrapped realm.
  //   layer c — patch the DOM INSERTION methods (appendChild / insertBefore /
  //             append) so the instant an <iframe> is CONNECTED we wrap its
  //             contentWindow.fetch SYNCHRONOUSLY — this catches a grab that
  //             reaches the realm WITHOUT touching the contentWindow property
  //             (e.g. `window.frames[i].fetch`), which layer b would miss.
  // Both are idempotent via the shared __sboFetchPatched flag (they and layer a
  // never double-wrap the same realm). Fail-soft throughout.
  function installIframeFetchBackstop() {
    // ---- layer b: contentWindow getter ----
    let desc;
    try {
      desc = Object.getOwnPropertyDescriptor(
        HTMLIFrameElement.prototype,
        "contentWindow"
      );
    } catch {
      desc = null;
    }
    if (desc && typeof desc.get === "function") {
      const origGet = desc.get;
      try {
        Object.defineProperty(HTMLIFrameElement.prototype, "contentWindow", {
          configurable: true,
          enumerable: desc.enumerable,
          get() {
            const win = origGet.call(this); // the REAL contentWindow
            wrapIframeRealm(win);
            return win;
          },
        });
      } catch {
        /* defineProperty failed (frozen prototype) — best-effort, skip */
      }
    }

    // ---- layer c: DOM-insertion hook ----
    function wrapNodeIfIframe(node) {
      try {
        if (node && node.tagName === "IFRAME") wrapIframeRealm(node.contentWindow);
      } catch {
        /* ignore */
      }
    }
    try {
      const origAppendChild = Node.prototype.appendChild;
      Node.prototype.appendChild = function (node) {
        const r = origAppendChild.call(this, node);
        wrapNodeIfIframe(node);
        return r;
      };
      const origInsertBefore = Node.prototype.insertBefore;
      Node.prototype.insertBefore = function (node, ref) {
        const r = origInsertBefore.call(this, node, ref);
        wrapNodeIfIframe(node);
        return r;
      };
      if (typeof Element !== "undefined" && Element.prototype.append) {
        const origElAppend = Element.prototype.append;
        Element.prototype.append = function (...nodes) {
          const r = origElAppend.apply(this, nodes);
          for (const n of nodes) wrapNodeIfIframe(n);
          return r;
        };
      }
    } catch {
      /* best-effort — a failed insertion patch just falls back to layer a/b */
    }
  }
  if (SITE.iframeFetchGrab && window === window.top) {
    installIframeFetchBackstop();
  }

  // --- WebSocket interception ----------------------------------------------
  // Two shapes share this patch (installed when the site is EITHER an exclusive
  // websocket-transport site OR an sse site with an additive `wsAnswer`
  // capability):
  //
  //   * copilot.microsoft.com (transport:"websocket") — the WHOLE transport is
  //     a WebSocket: the client sends a `send` frame with the prompt; the
  //     server streams `appendText` frames + a terminal `done`. We tap send
  //     (intervention) + message (capture).
  //
  //   * chatgpt-web (transport:"sse" + wsAnswer) — GROUNDED 2026-07-17: the
  //     prompt is redacted on the fetch/SSE leg, but the ANSWER + prompt echo +
  //     ids + model stream over the GLOBAL `ws.chatgpt.com` socket. We tap ONLY
  //     the server→client `message` events on that host and emit via the pure
  //     makeChatGPTWSAccumulator. No send-frame intervention (the prompt does
  //     not leave over this socket).
  //
  // Branch on the capability SHAPE (transport vs wsAnswer), never on a hostname.
  if (SITE.transport === "websocket" || SITE.wsAnswer) {
    const OrigWS = window.WebSocket;

    // attachWSAnswerTap wires the ADDITIVE wsAnswer capability onto an
    // already-constructed socket whose hostname matched SITE.wsAnswer.hostMatch.
    // The global socket MULTIPLEXES sequential/overlapping turns, so we keep a
    // Map of pure accumulators keyed by topic_id (== turn_id) and route each
    // frame ELEMENT (via SITE.wsAnswer.split) to THAT element's turn accumulator
    // — overlapping turns, even batched in one frame, stay fully independent
    // (HIGH/MED fixes, 2026-07-17 review). A per-topic `done` emits + evicts only
    // that topic. emitTurn is the same single chokepoint the copilot + SSE paths
    // use.
    function attachWSAnswerTap(ws, urlStr) {
      const make = SITE.wsAnswer.makeAccumulator;
      const split = SITE.wsAnswer.split;
      const turns = new Map(); // turnId -> { acc, startedAt, createdAt }
      const MAX_TURNS = 32;
      const TURN_TTL_MS = 10 * 60 * 1000; // evict a turn that never `done`s

      // evictStale drops abandoned turns SILENTLY — a turn with no `done` is not
      // a capture, so eviction (TTL or 33rd-topic overflow) must NEVER emit.
      function evictStale(now) {
        for (const [k, v] of turns) {
          if (now - v.createdAt > TURN_TTL_MS) turns.delete(k);
        }
        while (turns.size > MAX_TURNS) {
          turns.delete(turns.keys().next().value); // oldest-first (insertion order); drop, don't emit
        }
      }
      function getTurn(turnId, now) {
        let entry = turns.get(turnId);
        if (!entry) {
          entry = { acc: make(), startedAt: now, createdAt: now };
          turns.set(turnId, entry);
          evictStale(now);
        }
        return entry;
      }
      function emitAndClose(turnId, entry) {
        const acc = entry.acc;
        if (acc.finalize) {
          try {
            acc.finalize();
          } catch {
            /* best effort */
          }
        }
        const st = acc.state;
        turns.delete(turnId);
        // MED fix: an id-only `done` (has conversation_id/turn_id but NO prompt
        // AND NO answer text — a dropped/malformed encoded_item) is NOT a real
        // capture. Do not emit and do not disarm the idle heartbeat.
        if (!st.prompt && !st.text) return;
        // Cross-transport dedup (see recentEmits): the SSE leg may have already
        // emitted this same turn. Check/mark BOTH turn-scoped ids (topic turn_id
        // + working turn id) so a match on EITHER dedups.
        const keys = [st.messageId, st.workingTurnId, st.handoffId];
        if (wasRecentlyEmitted(keys)) return;
        markEmitted(keys);
        // WS-PRIMARY emit: the WS carries the full turn (prompt echo + answer +
        // conversation_id + turn_id + model). id_source resolves to "stream"
        // when a conversation id was harvested (emitTurn owns that).
        emitTurn(
          {
            url: urlStr,
            prompt: st.prompt,
            model: st.model,
            conversationId: st.conversationId,
          },
          {
            state: {
              text: st.text,
              conversationId: st.conversationId,
              messageId: st.messageId,
              model: st.model,
            },
          },
          entry.startedAt
        );
      }

      ws.addEventListener("message", (evt) => {
        try {
          if (typeof evt.data !== "string") return; // answer frames are text
          const now = performance.now();
          // PER-ELEMENT routing: a single frame may batch elements for more than
          // one turn, so route each element to its OWN topic's accumulator.
          for (const part of split(evt.data)) {
            if (part.topicId) {
              // A subscribe turn-start resets ONLY this topic's accumulator.
              if (part.turnStart) turns.delete(part.topicId);
              const entry = getTurn(part.topicId, now);
              entry.acc.feed(part.frame);
              if (entry.acc.state.done) emitAndClose(part.topicId, entry);
            } else if (part.completeConvId) {
              // Secondary belt-and-braces completion on the "conversations"
              // topic. It carries ONLY a conversation_id (NO turn_id), so
              // correlate to a SINGLE not-yet-emitted pending turn with that
              // conversation_id AND that ALREADY has answer text — never let a
              // conversation-complete close a fresh/empty pending turn of the
              // same conversation. Ambiguous (0 or >1) or no-text → ignore; the
              // per-topic `done` remains authoritative. WIRE CAVEAT:
              // conversation_id is NOT turn-unique.
              let matchId = "";
              let count = 0;
              for (const [tid, e] of turns) {
                if (e.acc.state.conversationId === part.completeConvId) {
                  matchId = tid;
                  count++;
                }
              }
              if (count === 1) {
                const e = turns.get(matchId);
                if (e.acc.state.text) {
                  e.acc.state.done = true;
                  emitAndClose(matchId, e);
                }
              }
            }
          }
        } catch {
          /* ignore malformed frame */
        }
      });
    }

    function SBOWebSocket(url, protocols) {
      const ws =
        protocols === undefined
          ? new OrigWS(url)
          : new OrigWS(url, protocols);
      const urlStr = String(url);

      // ADDITIVE wsAnswer sites (chatgpt-web): tap the matching answer host by
      // EXACT hostname equality (LOW fix — substring `.host.includes` would
      // match `ws.chatgpt.com.evil`). The answer socket host differs from the
      // page host; `.hostname` excludes any port.
      if (SITE.wsAnswer) {
        let match = false;
        try {
          match = new URL(urlStr).hostname === SITE.wsAnswer.hostMatch;
        } catch {
          /* ignore — non-URL socket */
        }
        if (match) attachWSAnswerTap(ws, urlStr);
        return ws;
      }

      // Only tap the Copilot chat socket (skip telemetry/other sockets, e.g.
      // the wps-picasso webpubsub notifications hub). EXACT hostname equality,
      // same hardening as the chatgpt wsAnswer match — a substring test would
      // also hit the literal anywhere in a foreign URL's path/query.
      let isChat = false;
      try {
        isChat = new URL(urlStr).hostname === "copilot.microsoft.com";
      } catch {
        /* ignore */
      }
      if (!isChat) return ws;

      // Pure parser owns the send/answer wire shapes (LIVE-CONFIRMED 2026-07-18,
      // copilot recon); this closure owns only the socket patch + intervention.
      let acc = makeCopilotAccumulator();
      let prompt = "";
      let sendModel = "";
      let startedAt = performance.now();
      let emitted = false; // one emit per turn (frames continue past `done`)
      // contested = this turn was begun while a prior turn's UNATTRIBUTABLE
      // frames were still inbound (the received-only interruption window). Such
      // a turn DROPS — it never emits — because the wire has no per-generation
      // correlator to prove which assistant frames are its own (re-review
      // HIGH #2, drop-over-corrupt; policy lives in copilotTurnDisposition).
      let contested = false;
      // socketTainted = STICKY per-SOCKET taint (re-review HIGH #2, wave-5).
      // Once ANY turn on THIS socket becomes contested, the socket is DROP-ONLY
      // for every subsequent turn — even one that looks uncontested because the
      // prior contested turn latched an (unreliable) id — until a real resync
      // boundary. This closure IS the per-socket state: a socket reconnect
      // builds a NEW SBOWebSocket → a fresh closure with socketTainted=false, so
      // the taint clears naturally on reconnect and nowhere else. Lives here (not
      // in parsers) because it is socket-lifetime state; the pure transition is
      // copilotTaintNext and the emit gate is copilotShouldEmit({...tainted}).
      let socketTainted = false;

      // priorMsgIds tracks the assistant message ids of COMPLETED turns so a
      // late straggler frame from turn A can't fold into turn B (adversarial
      // review r5-2). Bounded FIFO — a long chat must not grow it unboundedly.
      const priorMsgIds = new Set();
      const PRIOR_MSG_MAX = 64;
      function rememberPriorMsgId(id) {
        if (!id) return;
        priorMsgIds.add(id);
        while (priorMsgIds.size > PRIOR_MSG_MAX) {
          priorMsgIds.delete(priorMsgIds.values().next().value);
        }
      }

      // beginTurn resets the per-turn state at each `send` — one accumulator per
      // turn, the single owner of that turn's answer/ids.
      function beginTurn(model, capturedPrompt, convId) {
        // Drop-over-corrupt disposition (re-review HIGH #2): the pure
        // copilotTurnDisposition helper owns the policy for the PRIOR
        // not-yet-emitted turn. A CLEAN interrupt (prior latched an assistant
        // id — user stopped/regenerated after the id was known) → quarantine
        // that id so its stragglers can't latch/contaminate this fresh turn
        // (r5-2). A RECEIVED-ONLY interruption (prior saw its user echo but was
        // cut before ANY assistant id latched → its foreign frames are inbound
        // with an id we NEVER learned, and the wire has no correlator to
        // attribute them) → this turn is CONTESTED and will DROP: a missed
        // capture is preferred over pairing the prior turn's answer with this
        // turn's prompt. `deferLatchUntilReceived` is set on the same condition
        // as belt-and-braces (keep the orphan's frames from folding in before
        // the drop), but the DROP — not arrival order — is the correctness
        // guarantee. A turn that already emitted recorded its id at `done`.
        const disp = copilotTurnDisposition({
          emitted,
          latchedId: acc.latchedAnswerId(),
          sawReceived: acc.sawReceivedEcho ? acc.sawReceivedEcho() : false,
        });
        if (disp.quarantineId) rememberPriorMsgId(disp.quarantineId);
        // Sticky per-socket taint (re-review HIGH #2, wave-5): a contested prior
        // turn taints the socket FOR GOOD. The quarantine above still fires (so
        // the contested turn's latched id — possibly foreign — is skipped), but
        // a quarantined id must NOT clear the taint, so copilotTaintNext folds
        // OR-only (never clears). Every subsequent turn on this socket then
        // drops via the copilotShouldEmit `tainted` gate, closing the wave-4
        // "B contested → C uncontested → B's late frames emit under C" hole.
        socketTainted = copilotTaintNext(socketTainted, disp);
        prompt = capturedPrompt;
        sendModel = model;
        startedAt = performance.now();
        // Keep the accumulator in defer-latch mode while the socket is tainted:
        // the turn will drop regardless, but deferring the latch keeps a
        // straggler from mutating this accumulator's visible state pre-drop.
        acc = makeCopilotAccumulator({
          deferLatchUntilReceived: disp.contested || socketTainted,
        });
        contested = disp.contested;
        emitted = false;
        if (convId) acc.state.conversationId = convId;
      }

      const origSend = ws.send.bind(ws);
      ws.send = function (data) {
        try {
          if (typeof data === "string") {
            const j = JSON.parse(data);
            const frames = Array.isArray(j) ? j : [j];
            // Find the `send` frame ANYWHERE in the batch (adversarial review
            // r5-4): a batched envelope [control, send] must keep the control
            // frame, and a send at a non-zero index must still be recognized. We
            // rewrite the send element IN PLACE and re-serialize the FULL
            // envelope so sibling frames are never dropped.
            let sendIdx = -1;
            for (let k = 0; k < frames.length; k++) {
              const fr = frames[k];
              if (
                fr &&
                typeof fr === "object" &&
                (fr.event === "send" || fr.type === "send")
              ) {
                sendIdx = k;
                break;
              }
            }
            if (sendIdx !== -1) {
              const frame = frames[sendIdx];
              // idle-cancel deferred to emitTurn (LOW-2): a blocked/failed send
              // frame that never reaches `done` must leave the heartbeat armed.
              const parsed = parseCopilotSendFrame(frame);
              const rawPrompt = parsed.prompt;
              // Pre-send intervention on the WebSocket `send` frame — the
              // JS-call-layer intercept applies to individual WS messages,
              // which declarativeNetRequest cannot touch (§7.2).
              const res = applyIntervention(rawPrompt);
              if (res.block) {
                beginTurn("", "", "");
                return; // hard stop: never emit the frame
              }
              if (res.body !== rawPrompt) {
                // Redaction changed the prompt. Rewrite each text FIELD IN PLACE,
                // preserving every part's position + metadata AND all non-text
                // parts (rewriteCopilotContent, adversarial review r5-3), then
                // re-serialize the FULL batch (r5-4) — never drop siblings.
                const redact = (s) => applyIntervention(s).body;
                if (frame.content !== undefined) {
                  frame.content = rewriteCopilotContent(frame.content, redact);
                } else if (frame.text !== undefined) {
                  frame.text = rewriteCopilotContent(frame.text, redact);
                } else if (frame.message !== undefined) {
                  frame.message = rewriteCopilotContent(frame.message, redact);
                }
                const rebuilt = JSON.stringify(
                  Array.isArray(j) ? frames : frames[0]
                );
                const capturedField =
                  frame.content !== undefined
                    ? frame.content
                    : frame.text !== undefined
                      ? frame.text
                      : frame.message;
                beginTurn(
                  parsed.model,
                  copilotJoinContent(capturedField),
                  parsed.conversationId
                );
                return origSend(rebuilt);
              }
              beginTurn(parsed.model, rawPrompt, parsed.conversationId);
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
            // Drop a LATE frame belonging to an ALREADY-COMPLETED turn (r5-2):
            // after turn A `done`s and turn B's send reset the accumulator, an
            // A straggler (appendText / title) must not fold into B. The current
            // turn's own frames (id not yet completed) pass through untouched.
            if (
              f &&
              typeof f === "object" &&
              typeof f.messageId === "string" &&
              f.messageId &&
              priorMsgIds.has(f.messageId)
            ) {
              continue;
            }
            acc.feed(f);
            // Emit SYNCHRONOUSLY at `done` (adversarial review r5-5): the old
            // one-macrotask defer (to fold in the trailing `titleUpdate`) lost
            // the completed turn whenever the tab closed / navigated / was
            // discarded before the timer ran. Title arrives AFTER `done` and is
            // unused downstream, so we never wait for it. One emit per turn; the
            // assistant id is recorded so its own stragglers are dropped (r5-2).
            // copilotShouldEmit gates on done + not-yet-emitted + not contested
            // + not socket-tainted: a CONTESTED turn DROPS, and once the socket
            // is TAINTED every later turn DROPS too (re-review HIGH #2, wave-5,
            // drop-over-corrupt — sticky until socket reconnect).
            if (
              copilotShouldEmit({
                done: acc.state.done,
                emitted,
                contested,
                tainted: socketTainted,
              })
            ) {
              emitted = true;
              rememberPriorMsgId(acc.state.messageId);
              emitTurn(
                {
                  url: String(url),
                  prompt,
                  model: sendModel || acc.state.model,
                  conversationId: acc.state.conversationId,
                },
                { state: acc.state },
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
