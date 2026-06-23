# Teams & Org — Architecture

How the org feature works: the components, the runtime data flow, the security
and privacy model, and where each piece lives in the source tree. Pair this
with [teams-getting-started.md](teams-getting-started.md) (install) and
[teams-operations.md](teams-operations.md) (run).

---

## 1. Components

```
┌─────────────────────────────┐         ┌──────────────────────────────────────┐
│  Developer machine          │         │  observer-org server (one per org)     │
│                             │         │                                        │
│  observer (agent)           │         │  ┌──────────┐  SAML  ┌──────────────┐  │
│   • local SQLite            │         │  │  SAML SP │◀──────▶│   IdP        │  │
│   • proxy / watcher / hooks │         │  └────┬─────┘        │ (Okta/Entra/ │  │
│   • dashboard :8081         │         │       │              │  Google)     │  │
│                             │         │  ┌────▼─────┐  SCIM  └──────────────┘  │
│   internal/orgclient ───────┼────────▶│  │  SCIM    │◀───────  users/groups    │
│     enroll + signed push    │  HTTPS  │  └──────────┘                          │
│                             │  :8443  │  ┌──────────────┐                      │
│   internal/exporter/otel ───┼──┐      │  │  ingest      │ content-free rows    │
│     (optional, off default) │  │      │  │  + rollups   │──▶ server SQLite      │
└─────────────────────────────┘  │      │  └──────┬───────┘                      │
                                 │      │  ┌──────▼───────┐                      │
        OTLP/HTTP                │      │  │ org dashboard│ /api/org/* + SPA     │
        ▼                        │      │  └──────────────┘ (role-scoped)        │
┌─────────────────┐             │      └────────────────────────────────────────┘
│ OTel collector  │◀────────────┘
│ (your infra)    │   gen_ai.* + sbo.* spans
└─────────────────┘
```

Two independent "rails" leave the agent, both opt-in and both content-free:

1. **The org push rail** (`internal/orgclient` → server `ingest`): enrolment +
   signed, gzipped, content-free row batches. Drives the org dashboard.
2. **The OTel rail** (`internal/exporter/otel`): per-turn LLM spans to *your*
   collector. Needs only the agent — no org server.

---

## 2. The org push rail (runtime data flow)

### Enrolment

1. An admin mints a one-time token (`POST /api/org/enrolment-tokens`, or the
   `observer-org new-enrolment-token` CLI). Tokens are stored argon2id-hashed;
   the wire form is a compound `<token_id>.<secret>`.
2. The agent runs `observer enroll <url> <token>`. It generates an Ed25519
   keypair locally, POSTs the token + its public key, and the server (after
   verifying + burning the token) returns a 90-day **bearer** bound to that
   public key. The agent writes the bearer + signing key to the OS keychain
   (0600-file fallback on headless hosts), seeds its push cursor from the
   current max row ids, and only then records the enrolment row.

### Push loop

The agent's push loop (`orgclient.PushLoop`, an errgroup goroutine started by
`observer start` only when `[org_client] enabled`):

1. Selects rows inserted since the per-table cursor via the **single privacy
   seam** `store.SelectUnpushedSince`. Always carries the `*_hash` columns
   (`target_hash`, `source_file_hash`, `project_root_hash`,
   `git_remote_hash`); raw content/path columns (`target`, `source_file`,
   `project_root`, `git_remote`) are stripped per row unless the node has
   set `[org_client.share].full_content = true` (per-node opt-in; v1.8.0+).
   A `target_action_allowlist` ships raws for safe action types
   (`read_file`, `edit_file`, `write_file`) without enabling the full
   posture. A `[org_client.scope]` `project_root_allowlist` /
   `project_root_denylist` filters which projects feed the push at all.
2. Gzips the batch and signs `orgcontract.PushSigningMessage(ts, gzipBytes)`
   with the enrol-time Ed25519 key.
3. POSTs with `X-SBO-Timestamp` + `X-SBO-Agent-Signature` headers.
4. On `200`, advances the cursor and records the push; on `401/403` stops and
   surfaces an auth error; on `5xx`/network, backs off (250ms→30s, ±25%
   jitter) and retries. The loop **never** fails the host agent — every error
   path degrades gracefully (P1 isolation).

### Ingest + rollups

The server (`internal/orgserver/ingest`) verifies the bearer, then verifies
the per-push signature over the *exact* gzip bytes against the enrol-bound
public key (±300s skew; absent/invalid/stale → `401`), gunzips (with a
decompression-bomb guard), and upserts rows with `INSERT OR IGNORE` on
deterministic composite keys (idempotent — re-pushes are no-ops). Rollup
queries (`internal/orgserver/rollup`) aggregate spend/activity with a
read-through TTL cache and a single proxy-deduped spend definition shared by
the dashboard, budget list, and budget evaluator.

### Dashboard rollup surfaces (`/api/org/*`)

The admin dashboard (`web2/`) reads a family of content-free, role-scoped
rollups. All aggregate only non-content columns (counts, costs, token buckets,
http_status/error_class, durations, tool/model labels, `*_hash` dimensions) —
no `target`/`project_root`/`git_remote`/reasoning is ever read. Proxy-only
metrics (latency, cache, HTTP error class) degrade to a `token_usage` fallback
or an **honest labeled empty** when no `api_turns` were captured in the window
(the OpenCode-via-Azure-direct reality), never a fabricated zero.

| Endpoint | Rollup | Scope | Notes |
|---|---|---|---|
| `GET /api/org/overview` | `Overview` (+enrichment) | admin / lead | hero spend + deltas, 4-bucket tokens, cache efficiency, reliability split, error rate, latency (proxy-only), tool/model mix, activity-by-day + hour histogram |
| `GET /api/org/teams` · `/teams/{id}` | `Teams` / `TeamDetail` | admin / lead | per-team spend/activity; detail is aggregate-only |
| `GET /api/org/teams/{id}/developers` | `Developers` | admin / lead | **AUDITED** per-developer drill-down (`view_team_developers`) |
| `GET /api/org/people` | `People` | admin / lead / self | **AUDITED** org-wide per-developer leaderboard (`view_org_developers`); keys on the developer identity that already ships, so it lights up for a no-SCIM-groups org. Aggregate KPIs render un-audited; the **named table is gated behind the audited reveal click** |
| `GET /api/org/tools` · `/models` | `Tools` / `Models` | admin / lead | per-tool / per-model cost, token buckets, sessions, active devs, success rate, avg TTFT (proxy-only → 0) |
| `GET /api/org/activity` | `Activity` | admin / lead | cost/actions-by-day, stacked-by-tool, tokens-by-day buckets, hour-of-day + day-of-week×hour grids (dow derived in Go) |
| `GET /api/org/telemetry` | `Telemetry` | **admin-only** | native-console vendor analytics (below) |
| `GET /api/org/projects` · `/projects/{id}` | `Projects` / `ProjectDetail` | admin / lead | spend per git root + cross-team overlap |
| `GET /api/org/sessions` · `/sessions/{id}` | `Sessions` / `SessionDetail` | admin / lead / self | **AUDITED** scoped session list + per-session rollup (`view_org_sessions`); project identity is the hash, never the raw path |
| `GET /api/org/sessions/{id}/messages` | `SessionMessages` | admin / lead / self | **AUDITED (deeper)** captured OTel message bodies (`view_session_messages`); see below |

**Message-content viewer** (`GET /api/org/sessions/{id}/messages`,
`rollup.SessionMessages`) is the one surface that returns actual prose — the
native-OTel bodies (prompts, tool input/output) a node ALREADY shipped under its
own `full_content` / `admin_managed` opt-in (the `otel_content` table, agent
migration 048 / server 007). It is a **read of already-shipped data — no new
wire shape, no agent migration**; the strongest privacy guarantee (the never-read
body columns `raw_tool_input`/`raw_tool_output`/`preceding_reasoning`/
`error_message` are not selected by the push seam at all) is untouched. Because
it discloses actual content it is a **deeper** disclosure than the metadata
detail: expanding it writes a distinct `view_session_messages` audit row (vs the
metadata view's `view_org_sessions`). Scoped exactly like the session detail
(admin → all; lead → their teams; member → self); an out-of-scope/unknown id is
a `404`. `content` is present only where the node shared it (hash-only otherwise,
shown as "bodies not shared"); captured today only on Claude Code nodes with
native-OTel logging on, so it is honest-empty elsewhere. There is **no remote
toggle** for content — the admin-driven path is `admin_managed`, authored on the
node via provisioning, never a server force.

**Content retention** (the only server-side prune): admins can set
`[dashboard.content_retention] otel_content_days` (**default 0 = keep forever**);
a positive value runs a daily sweep (`internal/orgserver/retention`) that NULLs
`otel_content` bodies older than the horizon while **keeping `content_hash`** and
the row (so audit / dedup / re-push survive). This is scoped to message bodies
only; the broader `server.data_retention_days` knob is documented but not yet
enforced by a sweep.

**Native-console telemetry** (`GET /api/org/telemetry`, `rollup.Telemetry`) is a
separate, **admin-only** surface that aggregates the server-side
`cc_/codex_/copilot_analytics_daily` tables (migrations 008/009/010, populated
by the `internal/orgserver/{ccanalytics,codexanalytics,copilotanalytics}`
pollers). These tables are **server-side only — never on the agent push wire**.
Per vendor it reports cost (honoring the cross-vendor **unit trap** — USD summed
only within USD; Codex ChatGPT-Enterprise credits kept in a distinct
`credits_cost`), token buckets, Claude Code **accept rate**, Copilot **seat
utilization** (latest point-in-time snapshot, not summed across days), and
engagement counts. The rollup selects no `user_key` (the email/login actor
identity), so it discloses no identity. When no poller has populated any table
the response is `configured:false` and the page renders an honest
"not configured" empty state naming the per-vendor admin-key dependency.

---

## 3. The OTel exporter (second rail)

`internal/exporter/otel` tails the agent's `api_turns` table and emits one
`gen_ai.client` span per turn to a batching OTLP/HTTP endpoint. It is
**off unless `[exporter.otel] enabled = true`** — disabled means no OTLP
client and zero network calls (the solo-local invariant). Spans carry the
GenAI v1.41.0 `gen_ai.*` semantic conventions plus a SuperBased `sbo.*`
namespace (project, session, cost, tool, freshness, …). Privacy gating lives
in `otel.Attributes`: `sbo.org.id` is emitted only when enrolled, and
`sbo.user.email` only when enrolled **and** `emit_user_email` is set.
Reference Grafana/Datadog/Prometheus dashboards are under
`docs/exporters/otel/`.

---

## 3.5 The policy-bundle rail (server → agent, guard spec §14.2)

The third rail runs in the OPPOSITE direction from push: it distributes
the **org guard-policy bundle** — a TOML rule set in the guard's §4.4
policy-file format, Ed25519-signed by a dedicated **policy signing key**
(`[policy] signing_key_path`, distinct from the bearer key) — from the
server to every enrolled agent. It never widens content sharing
(§14.1): nothing flows back on this rail.

- **Authoring** is server-side: `observer-org policy keygen` once, then
  `observer-org policy publish --file bundle.toml` (validates with the
  same lint the agent loads with — a bundle that tries to RELAX
  built-in strictness is refused before signing), `policy list` /
  `policy show` for the version history (`org_policy_bundles`,
  server migration 004). Dashboard authoring with RBAC joins in G14.
- **Distribution**: agents GET `/api/v1/policy-bundle` (bearer-authed,
  versioned, ETag/304) once at `observer start` and every
  `[org_client].policy_poll_interval_seconds` (default 1h).
- **Verification** (the §14.2 acceptance gate, in
  `orgclient.FetchPolicyBundle`): Ed25519 signature over the canonical
  message → public key must match the pin recorded at enrolment in
  `guard_policy_state` (delivered as `org_policy_public_key` in the
  enroll response; pre-G13 enrolments pin trust-on-first-fetch) →
  monotonic version (downgrade protection) → org-layer lint. A pass
  atomically replaces the local cache (`[guard.rules].org_bundle`,
  default `~/.observer/org-policy-bundle.json`); ANY failure keeps the
  previous bundle and records an **R-205** guard event.
- **Merge**: the guard loads the cached bundle as its `org` policy
  layer — the §4.6 STRICTNESS FLOOR. Org rules/overrides may only
  escalate; user and project layers may tighten further but never
  relax below the floor. Unenrolling removes the cache (and the
  floor); the append-only pin history stays as audit.
- **Compat both directions**: pre-G13 servers 404 the endpoint and
  agents run local-only policy; guard-off or pre-guard agents never
  call it.

---

## 4. Security model

**Authentication paths.**
- *Humans* authenticate to the dashboard via **SAML** (`crewjam/saml`); the
  session is a self-contained HMAC-SHA256 cookie (12h TTL, no session store).
- *IdP SCIM clients* authenticate with a static SCIM bearer token, compared in
  constant time.
- *Agents* authenticate to the push endpoint with an **Ed25519 bearer** the
  server minted at enrolment (JWT-shaped envelope, but decoded by hand — one
  algorithm, one key type, no `alg` negotiation, no JWS library, no
  plaintext-`alg` vulnerability class). Each push additionally carries a
  per-request Ed25519 signature over the exact body.

**Defence in depth.**
- Enrolment tokens are one-time, argon2id-hashed, and expiring; their minted
  `jti` is recorded so admins can revoke an issued bearer (`issued_bearers` →
  `revoked_bearers`, checked by `VerifyBearer`).
- Per-push signatures bind every batch to the enrolled key and a timestamp
  (±300s skew window) so a replayed or forged body is rejected with `401`.
- The dashboard enforces **role scope per request in the handler**
  (`API.resolveScope`): admin (`dashboard.admin_emails`), team lead
  (`org_team_members.role='lead'`), or member (nothing). The handler never
  trusts the URL — an off-team request is `403`, an out-of-scope project id is
  `404`. Drill-down into an individual developer writes the `audit_log` *before*
  disclosure and refuses if the write fails.

**Privacy posture (structural, not promised; v1.8.0+).**
- `orgcontract` carries content-bearing columns (`target`, `source_file`,
  `project_root`, `git_remote`) **only as `omitempty`**, always alongside
  their `*_hash` counterparts (`target_hash`, `source_file_hash`,
  `project_root_hash`, `git_remote_hash`). The raw columns ship only when
  the node operator has set `[org_client.share].full_content = true` in
  their local config. **The org admin has no remote toggle for this.**
- The only SQL path that produces a push batch,
  `store.SelectUnpushedSince`, scans both shapes per row and strips raws
  in Go before append based on the node's `ShareOptions`. A
  `tests/invariant/privacy_test.go` test stuffs a unique secret into every
  string column (`raw_tool_input`, `raw_tool_output`, `preceding_reasoning`,
  `error_message`, `target`, `source_file`, `projects.root_path`,
  `projects.git_remote`, `token_usage.source_file`) and asserts none
  crosses the wire in metadata-only mode.
- A per-action allowlist (`target_action_allowlist`) lets the operator
  ship raws for safe action types (`read_file`, `edit_file`,
  `write_file`) without flipping `full_content` on. A
  `[org_client.scope]` `project_root_allowlist` / `project_root_denylist`
  filters which projects feed the push at all.
- Pre-v1.8.0 data: `observer-org scrub-content --all --confirm` purges
  any already-landed raws on the server while preserving the hash
  counterparts (so rollups and dedup continue to function). The
  server logs a startup WARN if leaked content is detected.
- Project identifiers can still be derived as the first 16 hex of
  `SHA-256(project_root)` — not reversible to a path. The full
  `project_root_hash` is what ships in metadata-only mode.
- No prompts, full command outputs, file contents, or full assistant
  responses are ever pushed or exported, regardless of share mode.

**Artifact trust.**
- The `observer-org` image is published to `ghcr.io/marmutapp/observer-org` and
  **cosign keyless-signed** by digest; each release also carries **CycloneDX
  SBOMs** and **SLSA Level 3 build provenance** (`multiple.intoto.jsonl`).
- The build runs on the private origin repo, so the provenance attests that
  builder identity — verify with `slsa-verifier --source-uri
  github.com/marmutapp/superbased-observer-private` (v2.7.0+). The exact
  `cosign verify` / `slsa-verifier` commands are in
  [Operations §4/§6](teams-operations.md).

---

## 5. Source map

| Concern | Package / path |
|---|---|
| Wire contract (single source of truth) | `internal/orgcontract` |
| Enrolment stamping into agent rows | `internal/identity` |
| Agent: enroll / push loop / policy poll / OTel | `internal/orgclient`, `internal/exporter/otel` |
| Policy bundle: serve + publish + history | `internal/orgserver/api/policy.go`, `cmd/observer-org/policy.go` |
| Server bootstrap + mux + middleware | `internal/orgserver` (`server.go`), `internal/orgserver/api` |
| Server DB + migrations | `internal/orgserver/db` |
| Auth: bearer / session / SAML / middleware | `internal/orgserver/auth` |
| SCIM 2.0 storage adapter | `internal/orgserver/scim` |
| Ingest (signed-push verification + upsert) | `internal/orgserver/ingest` |
| Rollups + scope + budgets + audit | `internal/orgserver/rollup`, `internal/orgserver/budget` |
| Org dashboard SPA | `web2/` → embedded in the org dashboard handler |
| OpenAPI contract + codegen | `docs/openapi/orgserver.yaml`, `make gen-openapi` / `verify-openapi` |
| Server CLI | `cmd/observer-org` |
| Deploy: Docker / compose / Helm | `Dockerfile.observer-org`, `deploy/observer-org/`, `charts/observer-org/` |

The agent's `.gen.go` client/server stubs are byte-isolated from the
dashboard's generated code (the dashboard gen package is `skip-prune: true`;
the agent gen configs are pruned), so dashboard schemas never leak into the
agent contract.

---

## 6. Invariants worth knowing

- **Solo-local UX is byte-identical** when org mode is off — guaranteed by
  `tests/invariant/`.
- **Versioning is unified**: agent and server ship at the same semver tag.
  Compatibility is "matching minor" by intent, but the v1.7.x ↔ v1.8.x
  boundary is explicitly cross-compatible: a v1.7.x agent pushing into a
  v1.8.x server lands rows with hashes computed on-the-fly server-side
  (`internal/orgserver/ingest/ingest.go::hashOrComputed`), and a v1.8.x
  agent's metadata-only push has every new key as additive (`omitempty`)
  so a v1.7.x server reads it without surprises.
- **Share mode is a per-node TOML opt-in.** No server-side knob flips
  it. Bypassing this contract via an "org policy" would be a feature
  mistake — capture in code review.
- **Single seam invariant.** `store.SelectUnpushedSince` is the only
  place that mints a wire row. Any new content-bearing column needs (a)
  the strip logic added at the seam, gated by `ShareOptions`, and (b)
  a sentinel added to `tests/invariant/privacy_test.go::allSentinels`
  so a future regression is loud.
- **The server is a SQLite singleton** — one writer, one persistent volume.
  Scale vertically, not horizontally (the Helm chart pins `replicas: 1` with a
  `Recreate` strategy for exactly this reason).
