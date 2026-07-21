// content-isolated.js — ISOLATED-world content script (Phase 1: ChatGPT).
//
// Holds extension logic, chrome.storage, and messaging. It CANNOT see the
// page's fetch (separate JS realm), so it receives captured turns from the
// MAIN-world interceptor over window.postMessage — validating the origin and
// the per-load nonce — then applies the granularity rule + client-side token
// estimate and forwards the turn to the service worker, which relays it to
// the native-messaging bridge.
//
// The wire contract it produces matches internal/adapter/browserchat's
// CapturedTurn (single-sourced in that package's doc.go + this repo's
// browser-extension/README.md). SchemaVersion is stamped so a version-skewed
// bridge is visible.

(() => {
  "use strict";

  const SCHEMA_VERSION = 1;
  const MSG_TYPE = "SBO_BROWSER_TURN";
  const HEALTH_TYPE = "SBO_BROWSER_HEALTH";

  // Every browser-chatbot site the extension knows. The isolated world
  // validates that a captured turn names one of these before relaying it (a
  // page can't spoof an arbitrary site). Kept in sync with the Go
  // browserchat.siteRules table + the manifest matches.
  const KNOWN_SITES = new Set([
    "chatgpt-web",
    "claude-web",
    "perplexity-web",
    "gemini-web",
    "copilot-web",
  ]);

  let nonce = null;

  // Runtime config. The inlined usage_only granularity is the FAIL-CLOSED FLOOR
  // that applies when NO daemon policy is reachable (service worker asleep, or
  // the daemon has never answered) — in that state no content crosses the wire
  // ("send less" beats "scrub more", §6). It is NOT a standing privacy contract:
  // once connected, the extension FOLLOWS the daemon's cached
  // [browser].granularity_ceiling (default `full`) via resolveGranularity below,
  // and it is that CONNECTED-DAEMON full capture to which the CWS Limited Use
  // obligation attaches — REVISIT the Limited Use disclosure before any store
  // publish. An explicit persisted user choice
  // (`chrome.storage.sync.set({granularity:"redacted"})`) is still clamped by
  // the daemon ceiling (min by rank — never send more than the daemon stores).
  // Per-site toggles let the user disable an individual site; a site absent from
  // `sites` is enabled (fail-open). Real builds read these from
  // chrome.storage.sync; defaults are inlined so the extension is functional
  // before the options page exists.
  //
  // intervention (proposal §7): the pre-send warn/redact/block control. It
  // runs in the MAIN world (that's where the transports are patched), so the
  // isolated world only READS the config and relays it down. Default `off` —
  // the least-surprising posture for an observability tool: it does nothing
  // to your outgoing messages unless you opt in. `types` filters which PII
  // classes trigger it (empty = all Tier-1 classes).
  const DEFAULTS = {
    enabled: true,
    granularity: "usage_only",
    sites: {},
    intervention: { mode: "off", types: [] },
  };

  // chrome.storage.local key the service worker caches the daemon's effective
  // browser policy under (granularity ceiling + enabled + per-site toggles).
  // Kept in sync with service-worker.js DAEMON_POLICY_KEY.
  const DAEMON_POLICY_LOCAL_KEY = "sbo_daemon_policy";

  // GRAN_RANK orders the granularity levels for the min() clamp: a higher rank
  // discloses more content. Kept in sync with the Go granularityRank table.
  const GRAN_RANK = { usage_only: 0, redacted: 1, full: 2 };

  // resolveGranularity is the ZERO-CONFIG granularity precedence, kept pure so
  // it is unit-testable under Node:
  //   userExplicit — the granularity the user EXPLICITLY persisted in
  //     chrome.storage.sync (null/undefined when they never set one; the
  //     inlined DEFAULTS value is NOT "explicit").
  //   daemonGranularity — the daemon's cached [browser].granularity_ceiling
  //     (sbo_daemon_policy.granularity), null/undefined when not fetched yet.
  //   fallback — the fail-closed default (DEFAULTS.granularity = usage_only).
  // Precedence: an explicit user choice is the REQUESTED level but is clamped
  // by the daemon ceiling — min() by rank, so the extension NEVER sends more
  // than the daemon stores. With no user choice, follow the daemon ceiling.
  // With neither (daemon unreachable, no cache), the fail-closed fallback.
  // Unknown values collapse to the fallback (rank-lookup miss).
  function resolveGranularity(userExplicit, daemonGranularity, fallback) {
    const fb = GRAN_RANK[fallback] !== undefined ? fallback : "usage_only";
    const userValid = GRAN_RANK[userExplicit] !== undefined;
    const daemonValid = GRAN_RANK[daemonGranularity] !== undefined;
    if (userValid && daemonValid) {
      return GRAN_RANK[userExplicit] <= GRAN_RANK[daemonGranularity]
        ? userExplicit
        : daemonGranularity;
    }
    if (userValid) return userExplicit;
    if (daemonValid) return daemonGranularity;
    return fb;
  }

  // getConfig resolves the effective runtime config. `enabled` / `sites` /
  // `intervention` come from the inlined DEFAULTS overlaid by chrome.storage.
  // sync (unchanged). `granularity` follows the zero-config precedence above:
  // an explicit persisted user choice clamped by the daemon's cached ceiling,
  // else the daemon ceiling, else the usage_only fail-closed default. Every
  // chrome.* access is individually guarded so the function still resolves
  // (to DEFAULTS-derived values) under Node or an asleep service worker.
  async function getConfig() {
    let stored = {};
    try {
      stored = await chrome.storage.sync.get(DEFAULTS);
    } catch {
      stored = {};
    }
    const base = { ...DEFAULTS, ...stored };

    // Sentinel get with NO defaults, so an unset key stays absent — the only
    // way to tell an EXPLICIT user granularity from the inlined default.
    let userExplicit = null;
    try {
      const sentinel = await chrome.storage.sync.get("granularity");
      if (
        sentinel &&
        typeof sentinel.granularity === "string" &&
        sentinel.granularity
      ) {
        userExplicit = sentinel.granularity;
      }
    } catch {
      /* fail-soft: treat as unset */
    }

    let daemonGranularity = null;
    try {
      const local = await chrome.storage.local.get(DAEMON_POLICY_LOCAL_KEY);
      const policy = local && local[DAEMON_POLICY_LOCAL_KEY];
      if (policy && typeof policy.granularity === "string") {
        daemonGranularity = policy.granularity;
      }
    } catch {
      /* fail-soft: no cached policy */
    }

    base.granularity = resolveGranularity(
      userExplicit,
      daemonGranularity,
      DEFAULTS.granularity
    );
    return base;
  }

  // Client-side token estimate. Phase-1 stub = chars/4 (rune-ish). The
  // INTENDED dependency is gpt-tokenizer (pure JS, OpenAI family) — swapped in
  // without a server change because the server labels every browser token
  // "estimated" regardless of who computed it. Always an estimate; never
  // authoritative.
  function estimateTokens(text) {
    if (!text) return 0;
    return Math.ceil(text.length / 4);
  }

  // --- client-side redaction orchestration (proposal §6) -------------------
  // Tier-1 (regex + checksum) runs synchronously in-realm via the SBOTier1
  // global (loaded before this script by the content_scripts ordering). Tier-2
  // (on-device NER) is requested from the offscreen document through the
  // service worker; it may be UNAVAILABLE (model not vendored) — in which case
  // we degrade to Tier-1-only. Redaction never blocks or throws; a failure
  // just means fewer spans removed, and the server scrub.Scrubber is the
  // backstop for anything that still crosses the wire.
  const Tier1 =
    (typeof globalThis !== "undefined" && globalThis.SBOTier1) || null;

  async function requestTier2(text) {
    if (!text) return [];
    try {
      const resp = await chrome.runtime.sendMessage({ __sbo: "ner", text });
      if (resp && Array.isArray(resp.spans)) return resp.spans;
    } catch {
      /* service worker asleep / offscreen unavailable — degrade */
    }
    return [];
  }

  // redactText returns the two-tier-redacted string. If Tier-1 is somehow
  // absent (module load order broke), it returns the input unchanged rather
  // than throwing — the server scrubber remains the backstop.
  async function redactText(text) {
    if (!text || !Tier1) return text || "";
    const t1 = Tier1.detect(text);
    const t2 = await requestTier2(text);
    const merged =
      t2 && t2.length ? Tier1.resolveOverlaps(t1.concat(t2)) : t1;
    return Tier1.applyRedaction(text, merged);
  }

  // applyGranularity is async because the `redacted` level runs the two-tier
  // pipeline before content leaves the browser.
  //   usage_only: strip content fields entirely (§5.3 — the primary control).
  //   redacted:   keep content, but Tier-1+Tier-2 redacted client-side.
  //   full:       keep raw content (server scrub.Scrubber is the backstop).
  // Token estimates are computed over the ORIGINAL text (what the user
  // actually sent to the model), not the redacted copy, so usage stays true.
  async function applyGranularity(turn, granularity) {
    const out = {
      schema_version: SCHEMA_VERSION,
      site: turn.site,
      conversation_id: turn.conversation_id,
      // capture_id is OPAQUE random per-turn METADATA (not content), so it
      // ALWAYS rides regardless of granularity — including usage_only, where
      // prompt_text/response_text are stripped. That is the whole point: the Go
      // side uses capture_id as the id-less fallback session/message key (a tier
      // below conversation_id/message_id), and that fallback must still exist
      // when content is gone. Never derive it from content, never drop it here.
      capture_id: turn.capture_id || "",
      // id_source is METADATA (provenance of conversation_id: none|request|
      // stream|resume), not content, so it always rides regardless of
      // granularity — same as conversation_id/message_id/capture_id. The Go
      // wire struct now carries an id_source field, so it is STORED (not just
      // dropped as unknown); the closed value set above is the contract.
      id_source: turn.id_source || "none",
      message_id: turn.message_id,
      model: turn.model,
      request_url: turn.request_url,
      // Prefer a real usage count the MAIN-world parser captured (Claude web,
      // when the stream carried usage — TODO(must-verify-live)); otherwise
      // fall back to the chars/4 estimate over the original text. The server
      // labels every browser token "estimated" regardless of the source.
      prompt_tokens_est: turn.prompt_tokens_est || estimateTokens(turn.prompt_text),
      response_tokens_est:
        turn.response_tokens_est || estimateTokens(turn.response_text),
      latency_ms: turn.latency_ms,
      captured_at: turn.captured_at,
      granularity,
      // NOTE: `title` is intentionally NOT forwarded on the wire payload. It is
      // a topic-bearing (content-ish) field, so it stays out of the emitted
      // shape — even under usage_only, where prompt/response are stripped — to
      // avoid leaking the conversation topic. The daemon's CapturedTurn.Title
      // tolerates its absence (zero value ""); do not add it here without a
      // matching privacy decision.
    };
    if (granularity === "redacted") {
      out.prompt_text = await redactText(turn.prompt_text || "");
      out.response_text = await redactText(turn.response_text || "");
    } else if (granularity === "full") {
      out.prompt_text = turn.prompt_text || "";
      out.response_text = turn.response_text || "";
    }
    return out;
  }

  // Under Node (unit tests) there is no `window`; export the pure-ish
  // transforms instead of attaching the page listener. In the browser `window`
  // is present, so the listener attaches exactly as before (additive guard).
  if (typeof module !== "undefined" && module.exports) {
    module.exports = { applyGranularity, estimateTokens, DEFAULTS, resolveGranularity };
    return;
  }

  window.addEventListener("message", async (event) => {
    if (event.source !== window || event.origin !== location.origin) return;
    const data = event.data;
    if (!data || typeof data !== "object") return;

    if (data.__sbo === "SBO_HELLO") {
      nonce = data.nonce; // learn the MAIN-world nonce for this load
      // Relay the intervention config DOWN to the MAIN world (it can't read
      // chrome.storage). The MAIN world verifies this nonce before trusting
      // it. Default `off` means the document_start race window is harmless.
      try {
        const cfg = await getConfig();
        window.postMessage(
          {
            __sbo: "SBO_CONFIG",
            nonce,
            intervention: cfg.intervention || DEFAULTS.intervention,
          },
          location.origin
        );
      } catch {
        /* fail-soft: MAIN world keeps its default (off) */
      }
      return;
    }

    // Per-site health ping — relay to the service worker so the daemon /
    // dashboard can surface last-successful-capture + shape-canary counts.
    if (data.__sbo === HEALTH_TYPE) {
      if (!nonce || data.nonce !== nonce || !data.health) return;
      try {
        chrome.runtime.sendMessage({ __sbo: "health", health: data.health });
      } catch {
        /* fail-soft */
      }
      return;
    }

    if (data.__sbo !== MSG_TYPE) return;
    if (!nonce || data.nonce !== nonce) return; // spoof guard
    if (!data.turn || !KNOWN_SITES.has(data.turn.site)) return;

    const cfg = await getConfig();
    if (!cfg.enabled) return;
    // Per-site toggle: a site explicitly set false is dropped in-browser
    // ("send less" — the primary control). Absent = enabled.
    if (cfg.sites && cfg.sites[data.turn.site] === false) return;

    const payload = await applyGranularity(data.turn, cfg.granularity);
    try {
      chrome.runtime.sendMessage({ __sbo: "capture", payload });
    } catch {
      /* service worker asleep / reloading — drop this turn, fail-soft */
    }
  });
})();
