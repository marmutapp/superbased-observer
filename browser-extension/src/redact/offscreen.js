// SPDX-License-Identifier: Apache-2.0
// Copyright (c) SuperBased. Part of SuperBased Observer.
//
// Tier-2 NER worker (proposal §6.1). Runs inside the offscreen document. Its
// ONLY job: answer `ner:run` messages with name/org/location spans from an
// on-device transformer NER model (Transformers.js + ONNX Runtime Web,
// WebGPU-accelerated with a WASM CPU fallback).
//
// SCAFFOLDING NOTE (honest): the real model weights are large and are fetched
// to IndexedDB at runtime (the montevive/openai-privacy-filter pattern), NOT
// bundled in the extension package. To keep the extension CWS-shippable and
// this repo lean, the Transformers.js build + model are OPERATOR-VENDORED
// into `browser-extension/vendor/` (see README "Enabling Tier-2"). Until they
// are present, `loadModel()` fails soft and every `ner:run` returns
// {unavailable:true} — Tier-2 GRACEFULLY DEGRADES to Tier-1-only. Redaction
// is never blocked by a missing model; it just loses the NER classes.
//
// This file NEVER imports an extension API beyond chrome.runtime messaging
// (offscreen docs are API-restricted by design) and NEVER makes a network
// call for weights against a remote host at load — env.allowRemoteModels is
// forced false, matching the zero-telemetry references.
"use strict";

// Candidate local (packaged/vendored) path for the Transformers.js ESM build.
// Absent by default → Tier-2 degrades. An operator who vendors the build drops
// it here and flips [browser] tier2 on.
const TRANSFORMERS_URL = "../../vendor/transformers/transformers.min.js";
// Default NER checkpoint (ONNX, quantized) — a small token-classification
// model. Also operator-vendored under vendor/models/.
const NER_MODEL = "Xenova/bert-base-NER";

// Map raw model entity groups → our redaction span types. Rule = row.
const ENTITY_TYPE = {
  PER: "person",
  PERSON: "person",
  ORG: "org",
  LOC: "location",
  GPE: "location",
  MISC: "misc",
};

let modelState = "unloaded"; // unloaded | loading | ready | unavailable
let nerPipeline = null;
let loadReason = "";

// loadModel attempts to bring up the Transformers.js NER pipeline exactly
// once. Any failure (build absent, weights absent, WebGPU+WASM both refused)
// resolves the state to "unavailable" — callers then fall back to Tier-1.
async function loadModel() {
  if (modelState === "ready" || modelState === "unavailable") return modelState;
  if (modelState === "loading") return modelState;
  modelState = "loading";
  try {
    // Dynamic import so a missing vendored build is a catchable error, not a
    // hard parse failure of this file.
    const mod = await import(
      /* webpackIgnore: true */ TRANSFORMERS_URL
    ).catch((e) => {
      throw new Error("transformers build not vendored: " + (e && e.message));
    });
    const { pipeline, env } = mod;
    if (env) {
      // Never phone home for weights; use the packaged/IndexedDB cache only.
      env.allowRemoteModels = false;
      if (env.backends && env.backends.onnx && env.backends.onnx.wasm) {
        // Let ORT pick WebGPU when available; WASM CPU is the fallback.
        env.backends.onnx.wasm.numThreads = 1;
      }
    }
    nerPipeline = await pipeline("token-classification", NER_MODEL, {
      // Aggregate sub-word tokens into whole-entity spans with char offsets.
      aggregation_strategy: "simple",
    });
    modelState = "ready";
  } catch (e) {
    loadReason = (e && e.message) || "unknown";
    modelState = "unavailable";
    // eslint-disable-next-line no-console
    console.info("[sbo] Tier-2 NER unavailable, degrading to Tier-1:", loadReason);
  }
  return modelState;
}

// runNER(text) → { spans } or { unavailable, reason }. spans are Tier-2
// shaped ({start,end,type,tier:2,confidence}) so the isolated world can
// merge them with the Tier-1 set via SBOTier1.resolveOverlaps.
async function runNER(text) {
  if (typeof text !== "string" || text.length === 0) return { spans: [] };
  const state = await loadModel();
  if (state !== "ready" || !nerPipeline) {
    return { unavailable: true, reason: loadReason || "model not loaded" };
  }
  try {
    const results = await nerPipeline(text);
    const spans = [];
    for (const r of results || []) {
      // Transformers.js aggregated output: {entity_group|entity, score,
      // start, end, word}. Only keep spans with real char offsets.
      const group = String(r.entity_group || r.entity || "").replace(
        /^[BILU]-/,
        ""
      );
      const type = ENTITY_TYPE[group.toUpperCase()];
      if (!type) continue;
      if (typeof r.start !== "number" || typeof r.end !== "number") continue;
      if (r.end <= r.start) continue;
      spans.push({
        start: r.start,
        end: r.end,
        type,
        tier: 2,
        confidence: typeof r.score === "number" ? r.score : 0.5,
      });
    }
    return { spans };
  } catch (e) {
    return { unavailable: true, reason: (e && e.message) || "inference error" };
  }
}

// Message protocol (the isolated world → service worker → HERE):
//   { __sbo: "ner:run", text }  →  { spans } | { unavailable, reason }
//   { __sbo: "ner:status" }     →  { state, reason }
// Only messages addressed to the offscreen worker are handled; everything
// else is ignored so the service worker's own listeners are unaffected.
chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (!msg || typeof msg !== "object") return;
  if (msg.__sbo === "ner:run") {
    runNER(typeof msg.text === "string" ? msg.text : "").then(sendResponse);
    return true; // async response
  }
  if (msg.__sbo === "ner:status") {
    sendResponse({ state: modelState, reason: loadReason });
    return false;
  }
  return; // not ours
});

// Warm the model opportunistically. If it's not vendored this is a no-op that
// records "unavailable" so the first real request degrades instantly.
loadModel();
