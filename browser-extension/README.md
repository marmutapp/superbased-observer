# SuperBased Observer — Browser Capture Extension

An opt-in MV3 Chrome/Edge extension that passively observes **your own** AI
chatbot web usage and relays a per-turn summary to your local `observer`
daemon. It **never re-originates a request** — the real browser makes the real
request; the extension only observes the streamed response — so it cannot
break the site it watches. Tokens are **always estimated** (no target UI
returns authoritative counts).

**Sites captured (Phase 2):**

| Site | Tool | Transport | Endpoint (private) | Parser status |
|---|---|---|---|---|
| ChatGPT | `chatgpt-web` | SSE | `POST /backend-api/conversation` (+ `/f/conversation`) | stable |
| Claude.ai | `claude-web` | SSE `content_block_delta` | `.../chat_conversations/{id}/completion` | stable |
| Perplexity | `perplexity-web` | SSE | `/rest/sse/perplexity_ask` | stable |
| Gemini | `gemini-web` | BatchExecute RPC | `/_/BardChatUi/data/batchexecute` | **best-effort / incomplete** |
| Copilot (consumer) | `copilot-web` | WebSocket frames | `copilot.microsoft.com` | best-effort (cf_clearance-gated) |

Design of record: `docs/plans/browser-extension-and-m365-copilot-proposal-2026-07-10.md`;
Phase-0 findings: `docs/plans/browser-extension-phase-0-spike-findings-2026-07-10.md`.

> **Gemini is the hardest site and is documented incomplete.** Google's
> BatchExecute RPC is a fragmented wrapper far harder to parse than SSE; the
> parser records whatever it can extract (often just the fact a turn happened
> + an estimate) rather than crashing. Treat `gemini-web` capture as
> best-effort pending operator-attended live verification.

## Architecture

```
<site> tab
  content-main.js      (MAIN world)  — monkeypatches fetch (SSE sites) OR
        │                              WebSocket (Copilot); SITE = a DATA row
        │ window.postMessage           (chatgpt/claude/perplexity/gemini/copilot)
        ▼
  content-isolated.js  (ISOLATED)    — nonce+origin guard, per-site toggle,
        │                              granularity rule, client token estimate
        │ chrome.runtime.sendMessage   + relays per-site health pings
        ▼
  service-worker.js                  — sendNativeMessage (turns) +
        │                              chrome.storage.local health (sbo_health)
        ▼
  native-messaging-host/host.js      — spawns `observer browser hook capture`
        │ payload on STDIN             (works even when the daemon is down)
        ▼
  observer browser hook  →  internal/adapter/browserchat  →  store.Ingest
```

Two content scripts share the same `matches` and both run at
`document_start`; they cannot share variables (separate JS realms) so they
cross via `window.postMessage` with an origin + per-load nonce guard. The
MAIN-world script executes regardless of the page CSP because it is the
extension's own bundled code (no `eval`, no remote `<script src>`).

**SITE = DATA.** In `content-main.js`, every per-site difference (host,
capture endpoints, request/response parsing) is a row in the `SITES` table.
SSE and WebSocket are genuinely different transports, so they get two
interceptors that both feed the same `emitTurn()` — a capability shape, not a
name branch.

## The wire contract (single-sourced)

The extension → native host → `observer browser hook` all agree on ONE JSON
schema, **defined authoritatively in
`internal/adapter/browserchat/doc.go`** (the `CapturedTurn` type). It is
versioned via `schema_version`. Fields: `schema_version`, `site`,
`conversation_id` (→ SessionID, required), `message_id`, `model`,
`request_url`, `prompt_text`/`response_text` (**only at redacted/full
granularity**), `prompt_tokens_est`/`response_tokens_est`, `latency_ms`,
`captured_at`, `granularity`, `title`.

**Default granularity is `usage_only`**: no prompt/response content ever
leaves the browser — the extension does not even construct those fields.
"Send less" beats "scrub more". Any content that *does* cross the wire (at
redacted/full granularity) still hits the server-side ingest scrub seam AND
the **daemon granularity ceiling** (`[browser].granularity_ceiling`): the
daemon downgrades anything above its configured ceiling before storing, so
the daemon — not the extension — is the final authority on what is stored.

## Outbound-PII screening (client-side, best-effort — Phase 3)

> **Assistive, not compliance-grade DLP.** This screens *your own* outbound
> data before it leaves the browser. It is best-effort and **will miss
> things** — the on-device NER is probabilistic and the pre-send intercept is
> only as strong as the transports it can patch. It is **not** airtight DLP,
> and it does **not** touch any AI vendor's safety, moderation, or usage
> features. Market and read it as *"AI-usage observability + local
> outbound-PII screening."*

For a browser rail, client-side redaction is the **primary** control, not a
nice-to-have: once the extension *sends* raw content, the server can't un-see
it. So **"send less" (extension-side minimization) beats "scrub more"
(server-side)** — the default `usage_only` granularity never even constructs
a content field. Redaction/intervention only apply when you opt into a
content-bearing granularity or turn intervention on.

### Two-tier redaction pipeline

| Tier | Detects | Tech | Where it runs |
|---|---|---|---|
| **Tier 1** | email, phone, SSN, credit-card (Luhn), IBAN, IPv4/IPv6, GitHub/OpenAI/AWS/Bearer keys | regex + checksum, <1 ms, confidence 1.0 | `src/redact/tier1.js` — pure JS, loaded into every realm |
| **Tier 2** | names, orgs, locations | on-device transformer NER (Transformers.js + ONNX Runtime Web, WebGPU→WASM CPU) | `chrome.offscreen` document (`offscreen.html` + `src/redact/offscreen.js`) |

**Tier-1 rule table** (`src/redact/tier1.js`, `RULES` — one row per class,
`{name, re, validator?}`; adding a class is one row, never a code branch):

| Type | Shape | Checksum / range validator |
|---|---|---|
| `email` | `local@domain.tld` | — |
| `credit_card` | 13–19 digits, space/dash grouped | **Luhn** (mod-10) |
| `ssn` | `NNN-NN-NNNN` | area ≠ 000/666/900–999, group ≠ 00, serial ≠ 0000 |
| `iban` | `CC NN …` 15–34 chars | **mod-97 == 1** (ISO 7064) |
| `ipv4` | dotted quad | every octet 0–255, no leading zeros |
| `ipv6` | hex groups | — |
| `phone` | `+? digits/sep` | 7–15 digits (E.164 bound) |
| `github_token` | `gh[pousr]_…` | — |
| `openai_key` | `sk-…` | — |
| `aws_access_key` | `AKIA…` | — |
| `bearer_token` | `Bearer …` | — |

Spans are checksum-gated (a bare 16-digit run is *not* redacted unless it
passes Luhn), then resolved into a sorted, non-overlapping set and replaced
with typed `[REDACTED:<type>]` placeholders. Run the JS unit harness with
`node src/redact/tier1.test.js` (29 assertions, zero deps).

### Tier-2 offscreen placement + graceful degradation

MV3 service workers have no DOM and are torn down when idle, so the NER model
lives in a **`chrome.offscreen` document** (reasons `DOM_PARSER` + `WORKERS`),
created on demand by the service worker. Message protocol:

```
isolated world  --chrome.runtime--> service worker --chrome.runtime--> offscreen doc
   {__sbo:"ner", text}                {__sbo:"ner:run", text}          runs the model
        ^-------------------- {spans} --------------------^----- {spans}|{unavailable}
```

**The real model weights are NOT bundled** (they are large and fetched to
IndexedDB at runtime per the `montevive/openai-privacy-filter` pattern). This
repo ships the *scaffolding*: the offscreen doc, the message protocol, and the
loader that dynamic-`import()`s an **operator-vendored** Transformers.js build
from `browser-extension/vendor/transformers/` + model under `vendor/models/`.
Until those are vendored, `loadModel()` fails soft and **Tier-2 degrades to
Tier-1-only** — redaction still runs, it just loses names/orgs/locations. No
network fetch for weights is ever made (`env.allowRemoteModels = false`).

### Pre-send intervention (warn / redact / block) — best-effort

The **same MAIN-world interception point** that reads outgoing bodies can also
act on them *before* the real network call — a JS-call-layer intercept
(`src/content-main.js`), so the MV3 no-blocking-`webRequest` rule (a
network-layer rule) does not apply. Config is read in the isolated world
(`chrome.storage`) and relayed to the MAIN world as a nonce-guarded
`SBO_CONFIG` message.

- **`off`** (default) — the least-surprising posture: does nothing to your
  sends. An observability tool should not silently rewrite/stop your messages.
- **`warn`** — advisory in-page overlay listing detected span types, then
  proceeds. (It does **not** hold the request pending a decision — a
  documented MV3 limit; treat it as a heads-up, not a gate.)
- **`redact`** — rewrites the outbound body, stripping detected PII, then
  sends the redacted body (so even the model receives less).
- **`block`** — drops the call (rejects `fetch`, aborts `XHR`, suppresses the
  WS frame). A hard stop that can disrupt the site's UX by design.

**Transports patched:** `fetch`, `XMLHttpRequest.send`, `WebSocket.send`, and
`navigator.sendBeacon`. Body rewrite happens *before the network call* (not on
the composer DOM), which also sidesteps `copilot.microsoft.com`'s Trusted
Types (`require-trusted-types-for 'script'`); the overlay uses
`createElement` + `textContent` only, so it too is Trusted-Types-safe.

**Honest MV3 limits (this is best-effort, not airtight DLP):**
- The guardrail is only as strong as the transports it patches. A **new
  transport**, or page code that grabs a transport reference **before our
  `document_start` patch lands**, silently defeats it.
- Only **string** request bodies are rewritable; a `Request`-object body is
  observed but not rewritten.
- `declarativeNetRequest` could add a coarse URL/host backstop but **cannot
  inspect a body** — not built here (noted as a possible future backstop).
- The composer DOM `submit`/`keydown` is a **secondary** signal only, never a
  sufficient hard block on its own.

This is a **separate, in-browser, best-effort surface** — distinct from the
Plane-B `internal/guard` egress policy (which governs the developer's own
coding-agent tool calls at a transport we control). Do not conflate them.

### Server-side scrub backstop

Anything that *does* cross the wire (at `redacted` or `full` granularity)
still hits the **ingest-time `scrub.Scrubber`** at the adapter/hook boundary
(`internal/adapter/browserchat` via `cmd/observer/browser.go::ingestBrowserTurn`).
It uses `Scrubber.String` — **not** `ScrubForward` (that's the proxy-outbound
path with the 214 KB-JSON footgun documented in CLAUDE.md). The DB never
stores unscrubbed content; the *primary* guarantee is still that at
`usage_only` there is nothing to scrub. Pinned by
`internal/adapter/browserchat.TestServerScrubBackstopFullGranularity`.

## Configurability

- **Per-site toggle** (`chrome.storage.sync` `sites.<tool> = false`) drops a
  site in-browser (the primary "send less" control).
- **Granularity** (`usage_only` | `redacted` | `full`) — default `usage_only`.
  At `redacted`, the two-tier client-side pipeline runs before content leaves
  the browser.
- **Intervention** (`chrome.storage.sync` `intervention.mode` =
  `off`|`warn`|`redact`|`block`, default `off`; `intervention.types` filters
  which PII classes trigger it, empty = all) — the pre-send guardrail. Read in
  the isolated world, relayed to the MAIN world.
- **Daemon ceiling** (`~/.observer/config.toml` `[browser]`): `enabled`,
  `granularity_ceiling` (clamps what is stored), per-site `[browser.sites]`
  backstop, optional `[browser.listener]` loopback HTTP receiver
  (`listen_addr` default `127.0.0.1:8821`), `retention_days`. LOCAL-ONLY —
  never distributed to an org.

## Per-site health

The isolated world relays a health ping (last-successful-capture ts, capture
count, empty/shape-canary count) that the service worker records in
`chrome.storage.local` under `sbo_health.<site>`. This is how endpoint churn
surfaces from telemetry rather than user reports (proposal §3.7).

## Permissions & host_permissions decision

`host_permissions` lists the **five exact chat origins** (no wildcards) — the
extension's declared single purpose, scoped to stay in the Chrome Web Store
standard review lane. **Sites beyond this initial set** should use
`optional_host_permissions` + `chrome.permissions.request()` (runtime "add
this site") to avoid re-triggering an in-depth review on every expansion.
`scripting` registers the MAIN-world script; `storage` holds toggles +
health + redaction/intervention config; `nativeMessaging` bridges to the
local `observer browser hook`; `offscreen` hosts the Tier-2 NER worker
(carries no separate warning text). `debugger` is **not** requested (top-tier
trust warning; reserved for an opt-in diagnostics mode).

### CWS listing-hygiene note (Limited Use, from 2026-08-01)

This extension screens *your own* outbound data — it is substantively the
**opposite** of AI-guardrail circumvention (it never modifies, suppresses, or
bypasses any vendor's safety features, content moderation, or usage limits).
Frame the **public listing / single-purpose statement** as *"AI-usage
observability + local outbound-PII (DLP) screening."* Keep internally-accurate
words — "guardrail," "intervention," "bypass," "jailbreak," "intercept" — **out
of the public listing/screenshots** (they are fine in code/comments). Under the
Chrome Web Store **Limited Use** rule (enforced 2026-08-01), collected data
must be strictly necessary to the disclosed single purpose; the data-use
declaration must plainly disclose that the extension "reads your AI chat
conversations," and the manifest → Dashboard data-use declarations → privacy
policy must agree exactly. One reviewer note: *"Passive observability +
outbound-PII-redaction tool; it does not modify, suppress, or bypass any AI
vendor's safety features, content moderation, or usage limits — it only
inspects/redacts data the user is about to send, before it leaves the
browser."*

## Install

`observer init` installs the per-browser native-messaging host manifest for
you as its **4th consent step** (detects Chromium-family browser profile
dirs; writes `com.superbased.observer.browser.json` into each browser's
`NativeMessagingHosts/`). After installing the published extension, set its
**Extension ID** in the manifest's `allowed_origins` (it is written with a
`REPLACE_WITH_EXTENSION_ID` placeholder until the id is known) and point
`"path"` at the host launcher.

Manual (developer / load-unpacked):

1. Build the observer binary (or use one on PATH).
2. Chrome → `chrome://extensions` → *Developer mode* → *Load unpacked* → this
   `browser-extension/` directory. Note the **Extension ID**.
3. Run `observer init` (writes the host manifest), then edit the written
   manifest: `"path"` → absolute path to
   `native-messaging-host/host-launcher.sh`, `"allowed_origins"` →
   `chrome-extension://<YOUR_EXTENSION_ID>/`. Ensure `node` is on PATH and set
   `OBSERVER_BIN` / `OBSERVER_CONFIG` in the launcher environment.
4. Use any supported site normally. Each completed turn appears in the
   observer dashboard's **Browser chatbots** group and Connected Tools matrix
   under its `*-web` tool, with an **estimated** (`est.`) token/cost figure.

## Honest limits

- Tokens are **estimates** (`chars/4` stub client-side; the intended
  dependency is `gpt-tokenizer` for OpenAI; the server labels them
  `estimated`/unreliable regardless).
- Every endpoint is a **private, undocumented** web-app API that changes
  without notice — the parsers fail soft and a per-site health canary flags
  churn.
- **Gemini** (BatchExecute RPC) is best-effort/incomplete; **Copilot**
  (WebSocket) is cf_clearance-gated and needs live verification.
- Client-side redaction + pre-send intervention (Phase 3) are **best-effort
  and assistive, not compliance-grade DLP**: Tier-1 is checksum-gated regex,
  Tier-2 NER is probabilistic (and degrades to Tier-1-only until the model is
  vendored), and the intercept is only as strong as the transports it patches.
  At the default `usage_only` granularity there is no content to redact.
- **Live in-browser redaction/intervention is operator-attended.** The Tier-1
  detector is unit-tested (`node src/redact/tier1.test.js`) and the server
  scrub backstop is Go-tested, but real in-page interception against a live
  composer + real ONNX model loading have **not** been exercised here.
- Completions routed through a page **service worker** would bypass the
  MAIN-world `fetch`/`WebSocket` patch (none observed for these sites today).
- **Live multi-site capture is operator-attended** — authenticated in-browser
  stream capture could not be exercised in an automated test; the server +
  wiring path is verified, the parsers are best-effort pending live checks.
