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
   seam** `store.SelectUnpushedSince` — which reads content-free columns only.
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

**Privacy posture (structural, not promised).**
- `orgcontract` is the single wire source of truth and has **no content
  fields** — the absence is enforced by the type system.
- The only SQL path that produces a push batch, `store.SelectUnpushedSince`,
  selects content-free columns only. A `tests/invariant/privacy_test.go` test
  stuffs a secret into every content column and asserts none crosses the wire.
- Project identifiers are the first 16 hex of `SHA-256(project_root)` — not
  reversible to a path.
- No prompts, command bodies, file contents, or tool outputs are ever pushed
  or exported.

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
| Agent: enroll / push loop / OTel | `internal/orgclient`, `internal/exporter/otel` |
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
- **Versioning is unified**: agent and server ship at the same semver tag;
  compatibility is "matching minor".
- **The server is a SQLite singleton** — one writer, one persistent volume.
  Scale vertically, not horizontally (the Helm chart pins `replicas: 1` with a
  `Recreate` strategy for exactly this reason).
