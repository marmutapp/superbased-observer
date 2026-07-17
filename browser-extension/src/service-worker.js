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
        lastAt: health.lastAt || Date.now(),
      };
      chrome.storage.local.set({ [key]: map });
    });
  } catch (e) {
    console.warn("[sbo] health record failed:", e && e.message);
  }
  relayHealthBeacon(health);
}

// relayHealthBeacon derives a compact ok/degraded status from the per-site
// counters and relays it to the native host as a type:"health" message. The
// host routes it to `observer browser hook health`. Best-effort: a missing
// host drops the beacon and logs — health must never surface an error to the
// page (the same posture as relayToHost).
function relayHealthBeacon(health) {
  const captures = health.captures || 0;
  const empties = health.empties || 0;
  // "ok" once anything has captured; "degraded" when only shape-canary trips
  // (empties) have been seen — the parser is likely stale for this site.
  let status = "ok";
  let reason = "";
  if (captures === 0 && empties > 0) {
    status = "degraded";
    reason = `shape canary tripped ${empties}x with no successful capture — parser may be stale`;
  }
  const beacon = {
    type: "health",
    site: health.site,
    status,
    reason,
    ts: health.lastAt || Date.now(),
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
  // Tier-2 NER request from the isolated content world. `ner:run` /
  // `ner:status` (offscreen-addressed) are intentionally NOT handled here so
  // this listener never intercepts the offscreen worker's own reply.
  if (msg.__sbo === "ner") {
    forwardNER(typeof msg.text === "string" ? msg.text : "").then(sendResponse);
    return true; // async response
  }
});
