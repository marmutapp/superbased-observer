// SPDX-License-Identifier: Apache-2.0
// Copyright (c) SuperBased. Part of SuperBased Observer.
//
// Runnable Tier-1 detector harness. Pure Node, zero deps:
//   node src/redact/tier1.test.js
// Exits non-zero on the first failed assertion. This is the JS-side unit
// coverage for the client-side redaction primary control (proposal §6.1).
"use strict";

const assert = require("assert");
const T1 = require("./tier1.js");

let passed = 0;
function ok(name, cond) {
  assert.ok(cond, "FAIL: " + name);
  passed++;
}

// Secret-shaped fixtures are assembled by concatenation so no contiguous
// secret literal lands in source (write-filter safety + avoids a real-looking
// credential in the repo).
const GH = "gh" + "p_" + "A".repeat(32);
const AWS = "AKIA" + "ABCDEFGHIJKLMNOP"; // 4 + 16
const SK = "s" + "k-" + "abcdef0123456789ABCDEF"; // > 16 body
const BEARER = "Bearer " + "aGVsbG8gd29ybGQ_dG9rZW4tdmFsdWU";

// --- positive detections ------------------------------------------------
function detectsType(text, type) {
  return T1.detect(text).some((s) => s.type === type);
}

ok("email", detectsType("ping me at jane.doe@example.com please", "email"));
ok("phone", detectsType("call +1 (415) 555-0132 today", "phone"));
ok("ssn valid", detectsType("ssn 123-45-6789 on file", "ssn"));
ok("ssn invalid area 000 rejected", !detectsType("000-12-3456", "ssn"));
ok("ssn invalid area 666 rejected", !detectsType("666-12-3456", "ssn"));

// Credit card — 4111 1111 1111 1111 is the canonical Luhn-valid Visa test PAN.
ok("credit_card luhn valid", detectsType("card 4111 1111 1111 1111", "credit_card"));
ok(
  "credit_card luhn invalid rejected",
  !detectsType("num 4111 1111 1111 1112", "credit_card")
);

// IBAN — DE89 3704 0044 0532 0130 00 is the canonical mod-97-valid example.
ok("iban mod97 valid", detectsType("iban DE89370400440532013000 here", "iban"));
ok(
  "iban mod97 invalid rejected",
  !detectsType("iban DE00370400440532013000 here", "iban")
);

ok("ipv4 valid", detectsType("host 192.168.1.42 up", "ipv4"));
ok("ipv4 out-of-range rejected", !detectsType("999.1.1.1", "ipv4"));
ok("ipv6", detectsType("addr 2001:0db8:85a3:0000:0000:8a2e:0370:7334", "ipv6"));

ok("github token", detectsType("token " + GH, "github_token"));
ok("aws access key", detectsType("key " + AWS, "aws_access_key"));
ok("openai key", detectsType("key " + SK, "openai_key"));
ok("bearer token", detectsType("auth " + BEARER, "bearer_token"));

// --- redaction / span geometry -----------------------------------------
const { redacted, spans } = T1.redact(
  "email jane@example.com and card 4111 1111 1111 1111 done"
);
ok("redact removes email literal", !redacted.includes("jane@example.com"));
ok("redact removes card literal", !redacted.includes("4111 1111 1111 1111"));
ok("redact inserts typed placeholder", redacted.includes("[REDACTED:email]"));
ok("redact returns spans", spans.length === 2);

// Non-overlapping, sorted invariant.
const many = T1.detect(
  "a jane@example.com b 192.168.1.1 c " + GH + " d 123-45-6789"
);
let sortedNoOverlap = true;
for (let i = 1; i < many.length; i++) {
  if (many[i].start < many[i - 1].end) sortedNoOverlap = false;
}
ok("spans sorted + non-overlapping", sortedNoOverlap);

// Empty / non-string inputs never throw.
ok("empty string → no spans", T1.detect("").length === 0);
ok("non-string → no spans", T1.detect(null).length === 0);
ok("clean text → unchanged", T1.redact("hello world").redacted === "hello world");

// Validators directly.
ok("luhn direct valid", T1.validators.luhnValid("4111111111111111"));
ok("luhn direct invalid", !T1.validators.luhnValid("4111111111111112"));
ok("iban direct valid", T1.validators.ibanValid("DE89370400440532013000"));
ok("ipv4 direct 255 ok", T1.validators.ipv4Valid("255.255.255.0"));
ok("ipv4 direct 256 bad", !T1.validators.ipv4Valid("256.0.0.1"));

console.log("tier1.test.js: " + passed + " assertions passed");
