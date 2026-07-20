// service-worker.js — coordinator (Phase 1).
//
// The service worker holds NO long-lived state (MV3 tears it down when idle).
// It receives captured turns from the isolated-world content script and
// relays each to the native-messaging host, which invokes
// `observer browser hook capture` with the payload on STDIN. Native messaging
// works even when no localhost port is open and even when the observer daemon
// is not running (the hook opens the DB directly), mirroring the CLI hook
// path.
//
// The native-messaging host id must match the installed host manifest's
// "name" (see native-messaging-host/com.superbased.observer.browser.json).

"use strict";

// Side-effect import: parsers.js is a UMD module that, when evaluated (as a
// classic content script OR an ESM side-effect import like this), assigns its
// API to globalThis.SBOParsers. No named export exists (adding one would break
// its classic content-script load), so we read the global after import. This
// keeps the health-beacon derivation DRY with the unit-tested parsers.js copy.
import "./parsers.js";
const P = (typeof globalThis !== "undefined" && globalThis.SBOParsers) || null;

const HOST_NAME = "com.superbased.observer.browser";

// Send one captured turn to the native host. We use a one-shot
// sendNativeMessage per turn (rather than a long-lived port) so an idle
// service-worker teardown never strands an open port. Best-effort: a missing
// or errored host drops the turn and logs — capture must never surface an
// error to the page.
function relayToHost(payload) {
  try {
    chrome.runtime.sendNativeMessage(HOST_NAME, payload, (resp) => {
      const err = chrome.runtime.lastError;
      if (err) {
        console.warn("[sbo] native host error:", err.message);
        return;
      }
      if (resp && resp.status && resp.status !== "ok") {
        console.warn("[sbo] native host status:", resp.status);
      }
    });
  } catch (e) {
    console.warn("[sbo] relay failed:", e && e.message);
  }
}

// --- daemon policy sync (zero-config granularity) ------------------------
// The daemon's [browser].granularity_ceiling is the SINGLE lever for how much
// a captured turn discloses — there is no options page and no manual console
// write. The service worker fetches the daemon's effective policy over the
// SAME native-messaging channel captured turns use (a type:"config" request)
// and caches it in chrome.storage.local under sbo_daemon_policy; the isolated
// content world reads that cache and sends at min(any explicit user choice,
// the daemon ceiling). Refreshed at startup + on a 5-minute alarm so a
// config.toml edit takes effect without reloading the extension.
const DAEMON_POLICY_KEY = "sbo_daemon_policy";
const POLICY_ALARM = "sbo-daemon-policy-refresh";
const POLICY_REFRESH_MINUTES = 5;

// fetchDaemonPolicy requests the daemon's browser policy from the native host
// and caches it (with a fetched_at stamp). Best-effort: a missing/errored host
// or a non-config reply leaves the previous cache in place — the isolated
// world falls back to the usage_only default when no cache exists (fail-
// closed, the privacy contract). Never surfaces an error to any page.
function fetchDaemonPolicy() {
  try {
    chrome.runtime.sendNativeMessage(HOST_NAME, { type: "config" }, (resp) => {
      const err = chrome.runtime.lastError;
      if (err) {
        console.warn("[sbo] daemon policy fetch error:", err.message);
        return;
      }
      if (!resp || resp.type !== "config") return;
      const policy = {
        granularity:
          typeof resp.granularity === "string" && resp.granularity
            ? resp.granularity
            : "usage_only",
        enabled: resp.enabled !== false,
        sites:
          resp.sites && typeof resp.sites === "object" ? resp.sites : {},
        degraded: resp.degraded === true,
        fetched_at: Date.now(),
      };
      try {
        chrome.storage.local.set({ [DAEMON_POLICY_KEY]: policy });
      } catch (e) {
        console.warn("[sbo] daemon policy cache failed:", e && e.message);
      }
    });
  } catch (e) {
    console.warn("[sbo] daemon policy fetch failed:", e && e.message);
  }
}

// Per-site health is recorded in chrome.storage.local under sbo_health.<site>
// so an options/popup surface can render last-successful-capture + shape-
// canary counts. It is ALSO relayed to the native host as a compact
// type:"health" beacon (over the SAME native-messaging channel captured turns
// use) so daemon-side failures are legible — a broken manifest or stale
// parser shows up in `observer browser health`, not just in-browser.
function recordHealth(health) {
  if (!health || !health.site) return;
  try {
    const key = "sbo_health";
    chrome.storage.local.get({ [key]: {} }, (stored) => {
      const map = (stored && stored[key]) || {};
      map[health.site] = {
        captures: health.captures || 0,
        empties: health.empties || 0,
        id_missing: health.id_missing || 0,
        lastAt: health.lastAt || Date.now(),
      };
      chrome.storage.local.set({ [key]: map });
    });
  } catch (e) {
    console.warn("[sbo] health record failed:", e && e.message);
  }
  relayHealthBeacon(health);
}

// deriveHealthBeaconFallback mirrors parsers.deriveHealthBeacon's ORIGINAL
// ok/degraded semantics + the idle passthrough, used only if the side-effect
// import of parsers.js failed to populate globalThis.SBOParsers. The richer
// "ok-degraded-id" tier is added only on the canonical (P) path — a failed
// import degrades to the prior two-state behavior, never breaks.
function deriveHealthBeaconFallback(health) {
  const captures = health.captures || 0;
  const empties = health.empties || 0;
  if (typeof health.status === "string" && health.status) {
    return {
      status: health.status,
      reason:
        health.status === "idle"
          ? "no capture wire events since page load"
          : "",
      priority: health.status === "idle" ? "low" : "normal",
    };
  }
  if (captures === 0 && empties > 0) {
    return {
      status: "degraded",
      reason: `shape canary tripped ${empties}x with no successful capture — parser may be stale`,
      priority: "normal",
    };
  }
  return { status: "ok", reason: "", priority: "normal" };
}

// relayHealthBeacon derives a compact status from the per-site counters and
// relays it to the native host as a type:"health" message. The host routes it
// to `observer browser hook health`. Additive over the original ok/degraded:
// an id-missing capture run → "ok-degraded-id"; an idle heartbeat → "idle"
// (both stored/printed verbatim by the Go reader; unknown counter fields are
// ignored). Best-effort: a missing host drops the beacon and logs — health
// must never surface an error to the page (the same posture as relayToHost).
function relayHealthBeacon(health) {
  const derived =
    P && typeof P.deriveHealthBeacon === "function"
      ? P.deriveHealthBeacon(health)
      : deriveHealthBeaconFallback(health);
  const beacon = {
    type: "health",
    site: health.site,
    status: derived.status,
    reason: derived.reason,
    ts: health.lastAt || Date.now(),
    // priority ranks an idle heartbeat ("low") below any real status
    // ("normal") so an idle background tab can't clobber a capturing tab's
    // status (MED-4). CONTRACT for the Go reader: a "low" beacon must NOT
    // overwrite a recent "normal" record for the same site. Additive/inert
    // until the Go side honors it (unknown field → ignored).
    priority: derived.priority || "normal",
    // Additive machine-readable counters — the Go reader ignores unknown
    // fields; a future reader distinguishes id-degraded from a stale parser.
    captures: health.captures || 0,
    id_missing: health.id_missing || 0,
  };
  try {
    chrome.runtime.sendNativeMessage(HOST_NAME, beacon, () => {
      const err = chrome.runtime.lastError;
      if (err) console.warn("[sbo] health beacon host error:", err.message);
    });
  } catch (e) {
    console.warn("[sbo] health beacon relay failed:", e && e.message);
  }
}

// --- Tier-2 offscreen NER coordinator ------------------------------------
// The service worker is the ONLY context that may create the offscreen
// document (proposal §6.1). The isolated content world cannot; it asks HERE
// via an `ner` message, we ensure the offscreen doc exists, forward the text
// to it, and relay the spans back. Everything fails soft: if the offscreen
// API is missing or the doc can't be created, we answer {unavailable:true}
// and the isolated world degrades to Tier-1-only redaction.
const OFFSCREEN_PATH = "offscreen.html";
let creatingOffscreen = null;

async function hasOffscreenDocument() {
  try {
    if (chrome.runtime.getContexts) {
      const ctx = await chrome.runtime.getContexts({
        contextTypes: ["OFFSCREEN_DOCUMENT"],
      });
      return Array.isArray(ctx) && ctx.length > 0;
    }
  } catch {
    /* fall through to the create-and-catch path */
  }
  return false;
}

async function ensureOffscreen() {
  if (!chrome.offscreen || !chrome.offscreen.createDocument) {
    throw new Error("offscreen API unavailable");
  }
  if (await hasOffscreenDocument()) return;
  if (creatingOffscreen) {
    await creatingOffscreen;
    return;
  }
  creatingOffscreen = chrome.offscreen
    .createDocument({
      url: OFFSCREEN_PATH,
      reasons: ["DOM_PARSER", "WORKERS"],
      justification:
        "Run on-device PII NER (Tier-2 redaction) off the DOM-less, idle-torn-down service worker.",
    })
    .catch((e) => {
      // A concurrent create wins the "single offscreen document" race — treat
      // an already-exists error as success.
      if (e && /single offscreen/i.test(String(e.message))) return;
      throw e;
    })
    .finally(() => {
      creatingOffscreen = null;
    });
  await creatingOffscreen;
}

// forwardNER ensures the offscreen doc and relays the text to it. Returns the
// offscreen worker's response, or {unavailable:true} on any failure.
async function forwardNER(text) {
  try {
    await ensureOffscreen();
    const resp = await chrome.runtime.sendMessage({ __sbo: "ner:run", text });
    return resp || { unavailable: true, reason: "no response" };
  } catch (e) {
    return { unavailable: true, reason: (e && e.message) || "offscreen error" };
  }
}

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg || msg.__sbo === undefined) return;
  if (msg.__sbo === "capture" && msg.payload) {
    relayToHost(msg.payload);
    sendResponse({ ok: true });
    return false; // synchronous ack
  }
  if (msg.__sbo === "health") {
    recordHealth(msg.health);
    sendResponse({ ok: true });
    return false;
  }
  // A content script asking for the cached daemon policy (granularity ceiling
  // + per-site toggles). Additive convenience — the isolated world usually
  // reads chrome.storage.local directly — so it never blocks capture.
  if (msg.__sbo === "policy") {
    try {
      chrome.storage.local.get({ [DAEMON_POLICY_KEY]: null }, (stored) => {
        sendResponse({ policy: (stored && stored[DAEMON_POLICY_KEY]) || null });
      });
    } catch {
      sendResponse({ policy: null });
    }
    return true; // async response
  }
  // Tier-2 NER request from the isolated content world. `ner:run` /
  // `ner:status` (offscreen-addressed) are intentionally NOT handled here so
  // this listener never intercepts the offscreen worker's own reply.
  if (msg.__sbo === "ner") {
    forwardNER(typeof msg.text === "string" ? msg.text : "").then(sendResponse);
    return true; // async response
  }
});

// Sync the daemon policy at service-worker startup (top-level) and keep it
// fresh on a 5-minute alarm — a service worker is torn down between events, so
// the alarm re-wakes it to re-pull the ceiling. Every chrome.* access is
// guarded so the module still loads in a non-extension context.
try {
  fetchDaemonPolicy();
} catch (e) {
  console.warn("[sbo] initial daemon policy fetch failed:", e && e.message);
}
try {
  if (chrome.alarms && chrome.alarms.create) {
    chrome.alarms.create(POLICY_ALARM, {
      periodInMinutes: POLICY_REFRESH_MINUTES,
    });
  }
  if (chrome.alarms && chrome.alarms.onAlarm) {
    chrome.alarms.onAlarm.addListener((alarm) => {
      if (alarm && alarm.name === POLICY_ALARM) fetchDaemonPolicy();
    });
  }
} catch (e) {
  console.warn("[sbo] daemon policy alarm setup failed:", e && e.message);
}
