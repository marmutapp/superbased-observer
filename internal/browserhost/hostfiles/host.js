#!/usr/bin/env node
// host.js — native-messaging stdio host for the SuperBased Observer browser
// extension (Phase 1).
//
// The browser launches this process and speaks Chrome's native-messaging wire
// protocol over stdio: each message is a 4-byte little-endian uint32 length
// prefix followed by that many bytes of UTF-8 JSON. For every captured-turn
// message the browser sends, this host invokes
//   observer browser hook <event>
// with the payload JSON on the child's STDIN — the same landing point the
// loopback listener funnels into. The <event> is "health" for a health
// beacon (payload.type === "health") and "capture" for every other message.
// It then replies {status:"ok"} (framed) so the extension's
// sendNativeMessage callback resolves.
//
// The observer binary is resolved from the OBSERVER_BIN env var (set in the
// host launcher or the environment); it defaults to "observer" on PATH. An
// optional OBSERVER_CONFIG env var is forwarded as --config so the hook writes
// to the same observer.db the daemon uses.
//
// This host never crashes on a bad message: parse/spawn errors are logged to
// stderr (which the browser surfaces in the extension's native-messaging
// error) and the loop continues.

"use strict";

const { spawn } = require("child_process");

const OBSERVER_BIN = process.env.OBSERVER_BIN || "observer";
const OBSERVER_CONFIG = process.env.OBSERVER_CONFIG || "";

function writeMessage(obj) {
  const json = Buffer.from(JSON.stringify(obj), "utf8");
  const header = Buffer.alloc(4);
  header.writeUInt32LE(json.length, 0);
  process.stdout.write(header);
  process.stdout.write(json);
}

// eventForPayload picks the `observer browser hook <event>` subcommand for a
// message. A health beacon (payload.type === "health") lands on the health
// event; everything else is a captured turn.
function eventForPayload(payload) {
  if (payload && payload.type === "health") return "health";
  return "capture";
}

function ingest(payload) {
  const args = ["browser", "hook", eventForPayload(payload)];
  if (OBSERVER_CONFIG) args.push("--config", OBSERVER_CONFIG);
  let child;
  try {
    child = spawn(OBSERVER_BIN, args, { stdio: ["pipe", "ignore", "inherit"] });
  } catch (e) {
    process.stderr.write(`[sbo-host] spawn failed: ${e && e.message}\n`);
    return;
  }
  child.on("error", (e) => {
    process.stderr.write(`[sbo-host] child error: ${e && e.message}\n`);
  });
  try {
    child.stdin.write(JSON.stringify(payload));
    child.stdin.end();
  } catch (e) {
    process.stderr.write(`[sbo-host] write to child failed: ${e && e.message}\n`);
  }
}

// Native-messaging frame reader: buffer stdin, peel 4-byte-length-prefixed
// JSON messages as they complete.
let buf = Buffer.alloc(0);

process.stdin.on("data", (chunk) => {
  buf = Buffer.concat([buf, chunk]);
  for (;;) {
    if (buf.length < 4) return;
    const len = buf.readUInt32LE(0);
    if (buf.length < 4 + len) return;
    const body = buf.subarray(4, 4 + len);
    buf = buf.subarray(4 + len);
    let payload;
    try {
      payload = JSON.parse(body.toString("utf8"));
    } catch (e) {
      process.stderr.write(`[sbo-host] bad message json: ${e && e.message}\n`);
      continue;
    }
    ingest(payload);
    writeMessage({ status: "ok" });
  }
});

process.stdin.on("end", () => process.exit(0));
