# Capture Harness — DevTools Console ground-truth tool

`capture-harness.js` is a **passive, console-only diagnostic**. You paste it
into the DevTools Console on a **logged-in** AI-chatbot site, send **one**
message, and it prints a JSON dump of the real authenticated request/response
stream shape. We use that dump to tune the extension's per-site parsers
(`browser-extension/src/content-main.js`) so they align to the
`CapturedTurn` wire shape (`internal/adapter/browserchat/browserchat.go`).

It is **not** shipped in the extension bundle and makes **no network calls**.
It only wraps `fetch` / `XMLHttpRequest` / `WebSocket` to observe requests to
the current site's completion endpoint(s) and `console.log`s a structural
summary for you to copy.

---

## What it captures per site

| Site | Host | Endpoint(s) observed | Transport |
|------|------|----------------------|-----------|
| ChatGPT | `chatgpt.com` | `/backend-api/conversation`, `/backend-api/f/conversation` | SSE (`data:` frames) — flags a possible `stream_handoff` / resume second-stream |
| Claude | `claude.ai` | `/api/organizations/.../chat_conversations/{id}/completion` | SSE (`event:` + `data:` frames) |
| Perplexity | `perplexity.ai` | `/rest/sse/perplexity_ask` | SSE (`data:` frames) |
| Gemini | `gemini.google.com` | `/_/BardChatUi/data/batchexecute` | BatchExecute RPC (`)]}'`-prefixed nested-array envelope) — **best-effort** |
| Copilot | `copilot.microsoft.com` | chat WebSocket | WS frames (`send` / `appendText` / `done`) |

For each captured turn it prints:

- `request_url` + `method`
- `request_body_key_structure` — the top-level keys + nested shape, with long
  string **values truncated to ~80 chars** (structure, not content)
- `first_frame_samples` — the first ~10 streamed frames, each truncated
- `best_effort_extraction` + `captured_turn_mapping` — its best guess at
  `{model, prompt_text_sample, response_text_sample, conversation_id,
  message_id, token fields}` **with the JSON PATH it pulled each from**
  (e.g. `data.message.content.parts[]`, `data.delta.text`). **The paths are
  the point** — they are what tune the parser.
- `flags` — parse errors, event-type histogram, the ChatGPT second-stream
  canary, raw-truncation, etc.

---

## Operator run instructions (same for every site)

1. Open the site in Chrome and **make sure you are logged in**.
2. Press **F12** (or Cmd/Ctrl+Shift+I) → **Console** tab.
3. Paste the **entire contents of `capture-harness.js`** and press Enter.
   You should see a green banner:
   `▶ SBO capture armed — site=… Send ONE message now.`
4. **Send ONE message** in the chat and let the full response finish
   streaming.
5. When the turn ends it **auto-prints** `===== SBO CAPTURE #0 … =====`
   followed by a JSON object. (You can also run `__sboDump()` any time.)
6. **Right-click the printed JSON → "Copy object"** (or select + copy the
   text), **redact the text samples** (see below), and send it back.
7. When done, run `__sboStop()` to restore the original functions.

### Console commands

- `__sboDump()` — reprint all captured turns as JSON.
- `__sboRaw(i)` — print the **full untruncated** raw response buffer of
  capture `i`. **Use this for Gemini** (see below).
- `__sboStop()` — restore original `fetch`/`XHR`/`WebSocket`, stop capturing.
- `window.__SBO` — the live state object; `window.__SBO.captures` holds the
  raw records.

---

## Site-specific notes

### Gemini — paste the full (redacted) raw envelope
Gemini is a **BatchExecute RPC**: the response is a `)]}'`-prefixed
nested-JSON-array envelope, not clean SSE. The harness's extraction is
**best-effort** (it pulls the longest decodable string literal) and has **no
stable JSON path yet**. After sending a message:

1. Run **`__sboRaw(0)`**.
2. Copy the printed raw envelope, **redact the actual prompt/response text**
   inside it (leave the array structure/positions intact so we can locate the
   text path), and send that along with the dump.

### ChatGPT — watch for the second-stream flag
ChatGPT may POST to **both** `/backend-api/conversation` and
`/backend-api/f/conversation`, and can hand off to a second stream. The
harness observes both endpoints and sets
`flags.possible_second_stream = true` if it sees a `stream_handoff` / `resume_`
token in the frames. If you see **two** `SBO CAPTURE #…` blocks for one
message, **copy both** — that is the handoff we need to model.

### Claude / Perplexity — clean SSE
These are the cleanest. The dump's `flags.event_types` histogram (Claude) and
the `first_frame_samples` show the `data:` event structure and event types
(`message_start`, `content_block_delta`, …). Just copy the dump.

### Copilot — WebSocket
The dump has a `ws_frames` object (`sent` / `received` frame samples) instead
of SSE frames. Copy the whole dump.

---

## How the dump maps to `CapturedTurn`

The `captured_turn_mapping` block in the dump is laid out to line up
field-for-field with the Go `CapturedTurn` struct, so tuning is a direct
comparison:

| CapturedTurn field | Dump field (and the parser it tunes) |
|--------------------|--------------------------------------|
| `conversation_id` | `conversation_id` + `conversation_id_path` → the per-site request/response conv-id extraction |
| `message_id` | `message_id` + `message_id_path` |
| `model` | `model` + `model_path` → `makeXxxAccumulator` model field |
| `prompt_text` | `prompt_text_sample` + `prompt_text_path` → `parseXxxRequest` |
| `response_text` | `response_text_sample` + `response_text_path` → `makeXxxAccumulator` text field |
| `prompt_tokens_est` / `response_tokens_est` | `token_fields_found[]` (paths of any `token`/`usage` field the server-side estimate could be replaced by) |
| `request_url` / `latency_ms` | `request_url` / `latency_ms` |
| `granularity` / `title` / `captured_at` | set by the extension, not the wire — not needed from the harness |

The `*_path` strings are what get copied into `content-main.js`'s per-site
`parseRequest` / `makeAccumulator` functions.

---

## Privacy / redaction

The dump **contains short samples of YOUR prompt and response text** (in the
`*_sample`, `*_raw`, and request-body-structure `sample=` fields). **Redact
those text fields before sharing.** The parser only needs the **structure and
the field paths**, not the content. The harness truncates samples to ~80–240
chars specifically so there is little to redact, but check before you copy.
The harness itself never transmits anything — it only prints to your console.
