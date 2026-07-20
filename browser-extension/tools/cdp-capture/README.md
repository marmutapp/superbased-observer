# sbo-capture — CDP completion-shape recorder (DEV TOOL)

`sbo-capture` attaches to a **running Chrome** over the Chrome DevTools Protocol
(CDP) and records the completion **request/response shapes** of the AI-chatbot
sites the browser extension parses, so we can validate the extension's per-site
stream parsers (`browser-extension/src/parsers.js`) against **real
authenticated traffic** — **without MITM** and **without installing the
extension**.

It is the CDP sibling of the console-only `capture-harness.js`. Same per-site
endpoint list, same CapturedTurn-aligned dump shape, same privacy posture
(truncate samples, redactable, no exfil). What CDP adds that a console harness
**cannot** see:

- **The Pro/Thinking second-leg WebSocket** (`wss://ws.chatgpt.com/…`
  `stream_handoff` → `encoded_item` frames) — a page-context harness that only
  wraps `fetch`/`WebSocket` can miss frames on a socket opened before it armed,
  and cannot see a socket opened from a **worker/service-worker** at all; CDP
  observes `Network.webSocketFrame*` on every socket in the tab **and**
  (via `Target.setAutoAttach` flatten mode) on every worker target too.
- **The Gemini model header** `x-goog-ext-525001261-jspb` (the model id is in a
  request HEADER, not the body) — captured verbatim so the hex→model-name table
  can be validated.
- **The full request post-body** even for large posts (via
  `Network.getRequestPostData`) and the **accumulated response body** via
  `Network.getResponseBody`, independent of page JS.

**It is NOT shipped in the extension bundle.** It makes **no outbound network
calls** — it talks ONLY to the local Chrome DevTools endpoint on
`127.0.0.1:<port>`.

---

## What it captures

| Site | Host | Endpoint(s) matched (substring) | Transport |
|------|------|----------------------------------|-----------|
| ChatGPT | `chatgpt.com` | `/backend-api/conversation`, `/backend-api/f/conversation` — **excluding** the `…/prepare` + `…/init` debounce siblings (+ **all** tab WebSockets for the thinking-tier conduit leg) | SSE (+ conduit/WS) |
| Claude | `claude.ai` | `/chat_conversations/…/completion` (both markers required) | SSE |
| Perplexity | `perplexity.ai` | `/rest/sse/perplexity_ask` | SSE |
| Gemini | `gemini.google.com` | `StreamGenerate` only (the plain `batchexecute?rpcids=…` activity/settings RPCs are **not** the chat turn and are ignored) | batchexecute RPC |
| Copilot *(bonus)* | `copilot.microsoft.com` | chat WebSocket | WS |

> **Why the excludes matter (2026-07-10 live capture).** The first CDP capture
> caught ChatGPT's `/f/conversation/prepare` (a 384-byte debounce pre-flight)
> and Gemini's `batchexecute?rpcids=ESY5D` (an activity-settings RPC) instead of
> the real completion turns — both are now excluded so "captured ✓" requires the
> actual chat endpoint. Gemini's StreamGenerate is a long stream whose
> `getResponseBody` can return empty; the tool also accumulates the body via
> `Network.streamResourceContent` + `Network.dataReceived` as a fallback.

For each captured turn it writes `<out>/<site>-<timestamp>.json` containing:
`request_url`, `method`, `request_headers_of_interest` (Gemini model header),
`request_body_key_structure` (keys + nested shape, long string values truncated
to ~80 chars), a `response` block (SSE frame samples / Gemini raw `)]}'` head,
each truncated ~240 chars, with a `total_length` note), and a
`captured_turn_mapping` best-effort extraction of `{model, prompt_text_sample,
response_text_sample, conversation_id, message_id, token_fields_found}` **each
with the JSON PATH / header it came from**. The paths are the point. A
second-leg WS writes a separate `<site>-ws-<timestamp>.json` with `ws_frames`.

It also always writes a single **`<out>/_urls.json`** diagnostic: every request
URL+method and every WebSocket URL seen across the page **and all
worker/service-worker targets**, matched by a capture rule or not, each tagged
with its `target_type` (`page` / `worker` / `service_worker`), a `matched`
flag, and a repeat `count`. **Query strings are stripped** (a `has_query` flag
notes their presence) so no auth tokens land in it. This is the map that reveals
the real ChatGPT conduit host/path and any worker-opened socket the matcher
missed — read it first when a leg goes uncaptured, then tune `sites.go`.

---

## Operator run flow (Windows)

The orchestrator drives this from WSL via `cmd.exe`/Task-Scheduler interop; the
manual flow is:

1. **Fully close Chrome** (or use a separate profile so you never touch your
   real cookie store).
2. Launch Chrome with remote debugging on a **fresh** profile:
   ```
   "C:\Program Files\Google\Chrome\Application\chrome.exe" --remote-debugging-port=9222 --user-data-dir=C:\sbo-debug-profile
   ```
3. In that Chrome window, **sign into** the four sites: chatgpt.com, claude.ai,
   perplexity.ai, gemini.google.com (and optionally copilot.microsoft.com).
   Open each in its own tab.
4. Run the recorder, pointing `--out` at a `/mnt/c`-readable Windows path:
   ```
   sbo-capture.exe --once --out C:\sbo-dumps
   ```
   It prints the tabs it attached to, then a `WAITING — send ONE message in
   each attached tab now.` banner.
5. **Send one message per site** and let each response finish streaming. Each
   site prints `captured ✓  →  <path>` as its turn lands. `--once` exits once
   every attached site has captured one turn.
   - **ChatGPT:** use a **NON-thinking** model (e.g. `gpt-5-6`, not
     `gpt-5-6-thinking`) to capture the classic SSE completion on
     `POST /f/conversation`. A **thinking** model streams the completion over a
     separate **conduit** second leg keyed by a `conduit_token`, NOT the SSE
     leg — see below.
6. The dumps are in `C:\sbo-dumps`, readable from WSL at `/mnt/c/sbo-dumps`.
   **Redact the `*_sample` / `*_head` / `request_body` fields** before sharing —
   only STRUCTURE + PATHS are needed.

### Flags

- `--port` (default `9222`) — the Chrome `--remote-debugging-port`.
- `--out <dir>` (default a temp dir) — where dumps are written.
- `--once` — capture one turn per attached site, then exit. Without it the
  tool keeps running; **Ctrl-C** stops it (dumps are written as they arrive).

### ChatGPT Pro/Thinking conduit second leg

A **thinking-tier** turn (`gpt-5-6-thinking`) streams the completion over a
separate **conduit** leg keyed by a `conduit_token` JWT (returned from the
excluded `/prepare` call), NOT the classic SSE. To catch it, send a
**Pro/Thinking** prompt and run **WITHOUT `--once`** (so the tool keeps
watching after the SSE POST lands and the conduit socket opens). The tool
captures **every** WebSocket in the ChatGPT tab (host-agnostic), so a conduit
WS is written to a separate `chatgpt-web-ws-<ts>.json` with `flags` set when
markers appear: `chatgpt_encoded_item`, `chatgpt_message_stream_complete`,
`conduit_token_seen`, `stream_handoff_seen`. The SSE leg dump also sets
`flags.possible_second_stream` / `conduit_token_seen` / `stream_handoff_seen`.

The conduit socket is frequently opened from a **worker / service-worker**, not
the page context. The tool now `Target.setAutoAttach`es (flatten mode) to every
worker and enables the Network domain on each attached child, so a
worker-opened conduit WS is caught the same as a page one. Every socket seen —
page or worker — is also listed in `_urls.json` with its `target_type`, so if a
conduit host still slips a matcher you can read the real host/path there.

> **Residual risk:** if the conduit leg is a plain **HTTP fetch** to an
> unknown host/path (not a WebSocket), the tool cannot know which fetch to
> record and will only capture whatever the first SSE leg carried — but the
> fetch's host/path will still appear in `_urls.json` (as a `page`- or
> `worker`-target request) to tune against. A NON-thinking model always gives
> the full SSE completion and is the reliable path.

---

## Dump JSON → parsers.js field-path map

`captured_turn_mapping` lines up field-for-field with the Go `CapturedTurn`
struct AND the `parsers.js` function it validates:

| Dump field (`captured_turn_mapping.*`) | Validates in `parsers.js` |
|---|---|
| `prompt_text_path` | `parseChatGPTRequest` / `parseClaudeRequest` / `parsePerplexityRequest` prompt extraction |
| `model_path` | request `model` key + `makeXxxAccumulator` model field (ChatGPT `metadata.model_slug`, Claude `message.model`) |
| `response_text_path` | `makeXxxAccumulator` text accumulation (ChatGPT sticky-delta `…content/parts/0`; Claude `delta.text` text_delta; Perplexity triple-decode `text→steps[]→FINAL.content→answer`; Gemini `wrb.fr` `candidate[1][0]`) |
| `conversation_id_path` / `message_id_path` | per-site id extraction |
| `token_fields_found[]` | any `token`/`usage` field a chars/4 estimate could be replaced by (Claude `message.usage.input_tokens`, `message_delta.usage.output_tokens`) |
| `request_headers_of_interest["x-goog-ext-525001261-jspb"]` | Gemini `parseGeminiRequest` model source (the header, not the body) |

The extraction logic in `extract.go` is a **direct Go port of `parsers.js`**, so
if a live capture's `*_path` differs from what the port produced, the parser
needs tuning. The `[must-verify-live]` items from
`docs/plans/browser-extension-stream-shapes-2026-07-10.md` are exactly what
these dumps ground.

---

## Privacy / no-exfil

- **Localhost only.** The tool fetches `http://127.0.0.1:<port>/json` and dials
  the per-tab `ws://127.0.0.1:<port>/devtools/page/…` DevTools sockets. There
  is **no other network I/O** in the code.
- **Truncated at the source.** Prompt/response samples are cut to ~80 chars,
  frame samples to ~240 chars; request bodies are reduced to key STRUCTURE with
  long string values replaced by `<str:N chars>` markers.
- **Redactable.** Every dump leads with a `// PRIVACY` note naming the fields to
  redact (`*_sample` / `*_head` / `request_body`). The tool never transmits
  anything — it only writes local files you review before sharing.

---

## Build

Pure-Go, no CGO, its **own `go.mod`** (`module sbo-capture`) — it does not touch
the main repo module.

```bash
cd browser-extension/tools/cdp-capture
go test ./...                                   # table-driven tests, no live Chrome
GOOS=windows GOARCH=amd64 go build -o sbo-capture.exe .   # cross-compile for Windows
# native build also works:  go build ./...
```

The pure extraction/parse helpers in `extract.go` (the parsers.js port) are
covered by `extract_test.go` with synthetic CDP payloads (~90% per-func) — no
Chrome needed. The CDP client / capture state machine (`cdp.go`, `capture.go`,
`main.go`) require a live Chrome and are exercised by the operator run.
