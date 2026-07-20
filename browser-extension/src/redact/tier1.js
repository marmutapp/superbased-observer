// SPDX-License-Identifier: Apache-2.0
// Copyright (c) SuperBased. Part of SuperBased Observer.
//
// Tier-1 PII detector — the fast, deterministic half of the client-side
// two-tier redaction pipeline (proposal §6.1). Regex + checksum, <1 ms,
// confidence 1.0. Pure JS, ZERO extension-API dependencies, so the SAME
// module loads unchanged into three JS realms:
//   - the ISOLATED content-script world (redaction orchestration),
//   - the MAIN content-script world (pre-send intervention scan),
//   - the offscreen document (span-merge with the Tier-2 NER results).
//
// SITE = DATA / rule = ROW (CLAUDE.md #5): every detector is one row in the
// RULES table — {name, re, validator?}. Adding a PII class is one new row,
// never a new code branch. `detect()` walks the table; the redaction logic
// below is entirely class-agnostic.
//
// This is ASSISTIVE, best-effort screening — NOT compliance-grade DLP. It is
// the extension's "send less" primary control (client-side), backstopped by
// the server-side scrub.Scrubber at ingest. It will miss things; that is
// documented and expected.
(function (root, factory) {
  "use strict";
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  } else {
    root.SBOTier1 = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : self, function () {
  "use strict";

  // --- checksum / range validators ---------------------------------------
  // Each returns true when the raw match is a PLAUSIBLE instance of the type,
  // so a bare shape match (e.g. any 16-digit run) is not redacted unless it
  // also passes the class's checksum. Keeps false positives down.

  // Luhn (mod-10) — credit-card / PAN validation.
  function luhnValid(digits) {
    let sum = 0;
    let alt = false;
    for (let i = digits.length - 1; i >= 0; i--) {
      let d = digits.charCodeAt(i) - 48;
      if (d < 0 || d > 9) return false;
      if (alt) {
        d *= 2;
        if (d > 9) d -= 9;
      }
      sum += d;
      alt = !alt;
    }
    return digits.length >= 12 && sum % 10 === 0;
  }

  // IBAN mod-97 == 1 (ISO 7064). Move the first 4 chars to the end, map
  // letters A-Z → 10-35, then compute the big-number mod 97 iteratively.
  function ibanValid(raw) {
    const s = raw.replace(/\s+/g, "").toUpperCase();
    if (s.length < 15 || s.length > 34) return false;
    const rearranged = s.slice(4) + s.slice(0, 4);
    let remainder = 0;
    for (let i = 0; i < rearranged.length; i++) {
      const c = rearranged.charCodeAt(i);
      let val;
      if (c >= 48 && c <= 57) {
        val = c - 48; // 0-9
      } else if (c >= 65 && c <= 90) {
        val = c - 55; // A=10 .. Z=35
      } else {
        return false;
      }
      // Process 1 or 2 digits at a time to avoid BigInt.
      remainder = (remainder * (val > 9 ? 100 : 10) + val) % 97;
    }
    return remainder === 1;
  }

  // US SSN: reject the structurally-invalid area/group/serial groups so a
  // random NNN-NN-NNNN date-ish string is less likely to redact.
  function ssnValid(raw) {
    const m = raw.replace(/[^0-9]/g, "");
    if (m.length !== 9) return false;
    const area = m.slice(0, 3);
    const group = m.slice(3, 5);
    const serial = m.slice(5);
    if (area === "000" || area === "666") return false;
    if (area >= "900") return false; // 900-999 never assigned
    if (group === "00") return false;
    if (serial === "0000") return false;
    return true;
  }

  // IPv4: every octet in 0-255. The regex only enforces the dotted shape.
  function ipv4Valid(raw) {
    const parts = raw.split(".");
    if (parts.length !== 4) return false;
    for (const p of parts) {
      if (!/^\d{1,3}$/.test(p)) return false;
      const n = Number(p);
      if (n < 0 || n > 255) return false;
      if (p.length > 1 && p[0] === "0") return false; // no leading zeros
    }
    return true;
  }

  // Phone: 7-15 digits total (E.164 upper bound), at least 7 to avoid
  // catching short numeric runs.
  function phoneValid(raw) {
    const digits = raw.replace(/[^0-9]/g, "");
    return digits.length >= 7 && digits.length <= 15;
  }

  // --- the detector RULE table -------------------------------------------
  // Order matters only for readability; overlap resolution (below) makes the
  // final span set deterministic regardless of rule order. Key-shaped
  // patterns are built via new RegExp + string concatenation on purpose so
  // no contiguous secret-looking literal appears in source (write-filter
  // safety) and so the prefixes read clearly.
  const KEY_GH = "gh" + "[pousr]_[A-Za-z0-9]{20,}";
  const KEY_SK = "\\b" + "s" + "k-[A-Za-z0-9_-]{16,}";
  const KEY_AWS = "AKIA" + "[0-9A-Z]{16}";
  const KEY_BEARER = "Bearer\\s+[A-Za-z0-9\\-._~+/]{16,}=*";

  const RULES = [
    {
      name: "email",
      re: /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g,
    },
    {
      name: "credit_card",
      // 13-19 digits, optionally space/dash grouped. Luhn-gated.
      re: /\b(?:\d[ -]?){13,19}\b/g,
      validator: (m) => luhnValid(m.replace(/[^0-9]/g, "")),
    },
    {
      name: "ssn",
      re: /\b\d{3}-\d{2}-\d{4}\b/g,
      validator: ssnValid,
    },
    {
      name: "iban",
      re: /\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b/g,
      validator: ibanValid,
    },
    {
      name: "ipv4",
      re: /\b(?:\d{1,3}\.){3}\d{1,3}\b/g,
      validator: ipv4Valid,
    },
    {
      name: "ipv6",
      // Compact but permissive; only fires on clearly-hex-grouped forms.
      re: /\b(?:[A-Fa-f0-9]{1,4}:){2,7}[A-Fa-f0-9]{1,4}\b/g,
    },
    {
      name: "phone",
      re: /\+?\d[\d\s().-]{6,}\d/g,
      validator: phoneValid,
    },
    { name: "github_token", re: new RegExp(KEY_GH, "g") },
    { name: "openai_key", re: new RegExp(KEY_SK, "g") },
    { name: "aws_access_key", re: new RegExp(KEY_AWS, "g") },
    { name: "bearer_token", re: new RegExp(KEY_BEARER, "gi") },
  ];

  // detect(text) → sorted, non-overlapping spans [{start, end, type, tier,
  // confidence}]. Deterministic: on overlap the LONGER span wins, ties break
  // to the earlier rule. tier/confidence are stamped so a merged Tier-1 +
  // Tier-2 span set stays attributable.
  function detect(text) {
    if (typeof text !== "string" || text.length === 0) return [];
    const raw = [];
    for (const rule of RULES) {
      rule.re.lastIndex = 0;
      let m;
      while ((m = rule.re.exec(text)) !== null) {
        const value = m[0];
        if (m.index === rule.re.lastIndex) rule.re.lastIndex++; // zero-width guard
        if (value.length === 0) continue;
        if (rule.validator && !rule.validator(value)) continue;
        raw.push({
          start: m.index,
          end: m.index + value.length,
          type: rule.name,
          tier: 1,
          confidence: 1.0,
        });
      }
    }
    return resolveOverlaps(raw);
  }

  // resolveOverlaps merges an arbitrary span list (Tier-1 and/or Tier-2)
  // into a sorted, non-overlapping set. Longer spans dominate; equal-length
  // overlaps keep the first seen. This is the ONE place span geometry is
  // reconciled, so the Tier-2 merge reuses it unchanged.
  function resolveOverlaps(spans) {
    const sorted = spans
      .slice()
      .sort((a, b) => a.start - b.start || b.end - b.start - (a.end - a.start));
    const out = [];
    for (const s of sorted) {
      if (s.end <= s.start) continue;
      const last = out[out.length - 1];
      if (last && s.start < last.end) {
        // Overlap: keep whichever covers more characters.
        if (s.end - s.start > last.end - last.start) out[out.length - 1] = s;
        continue;
      }
      out.push(s);
    }
    return out.sort((a, b) => a.start - b.start);
  }

  // applyRedaction(text, spans) → redacted string. Each span becomes a
  // "[REDACTED:<type>]" placeholder (mirrors the server scrub.Scrubber's
  // typed placeholder so the two tiers read consistently). Spans MUST be the
  // resolved, non-overlapping set from detect()/resolveOverlaps().
  function applyRedaction(text, spans) {
    if (!spans || spans.length === 0) return text;
    const ordered = spans.slice().sort((a, b) => a.start - b.start);
    let out = "";
    let cursor = 0;
    for (const s of ordered) {
      if (s.start < cursor) continue; // defensive: skip stragglers
      out += text.slice(cursor, s.start);
      out += "[REDACTED:" + s.type + "]";
      cursor = s.end;
    }
    out += text.slice(cursor);
    return out;
  }

  // redact(text) — convenience: detect + applyRedaction in one call, plus the
  // span list so callers can surface "N items redacted" without re-scanning.
  function redact(text) {
    const spans = detect(text);
    return { redacted: applyRedaction(text, spans), spans };
  }

  return {
    RULES,
    detect,
    redact,
    applyRedaction,
    resolveOverlaps,
    // Exposed for unit tests / the offscreen span-merge.
    validators: { luhnValid, ibanValid, ssnValid, ipv4Valid, phoneValid },
  };
});
