# Teams & Org — Operations

Running `observer-org` day to day: backup/restore, key rotation, upgrades, and
troubleshooting. See [teams-getting-started.md](teams-getting-started.md) to
install and [teams-architecture.md](teams-architecture.md) for how it works.

---

## 1. What state exists

| State | Location | Backup priority |
|---|---|---|
| Server database | `/var/lib/observer-org/server.db` (+ `-wal`, `-shm`) | **Critical** — all org/identity/audit data |
| Config | `/etc/observer-org/config.toml` | Low (reproducible) |
| Bearer signing key | `bearer-signing.key` | **Critical** — losing it invalidates every agent bearer |
| Session HMAC key | `session.key` | Low (rotating just logs everyone out) |
| SAML SP cert/key | `sp.crt` / `sp.key` | Medium (rotation needs IdP re-trust) |
| SCIM token | `scim-token` | Medium (rotation needs IdP update) |

The server uses SQLite in WAL mode (pure-Go `modernc.org/sqlite`, no CGO).

---

## 2. Backup and restore

### Backup (online, consistent)

Use SQLite's online backup so you capture a consistent snapshot even while the
server is running:

```bash
# Docker:
docker exec observer-org sh -c \
  'sqlite3 /var/lib/observer-org/server.db ".backup /var/lib/observer-org/backup.db"' \
  || true   # distroless has no shell — see the volume-copy fallback below
```

The distroless image has no shell, so prefer one of:

- **Stop-and-copy** (brief downtime): `docker stop observer-org`, copy
  `/var/lib/observer-org/server.db*` (all three files) from the volume, then
  start again.
- **Volume snapshot**: snapshot the underlying PV/volume. With WAL mode, copy
  `server.db`, `server.db-wal`, and `server.db-shm` together.
- **Sidecar**: run a small `sqlite3`-equipped sidecar mounting the same volume
  read-only and run `.backup`.

On Kubernetes, snapshot the PVC (VolumeSnapshot) or run a backup CronJob with a
sidecar that mounts the claim.

### Restore

1. Stop the server.
2. Replace `/var/lib/observer-org/server.db` with the backup (remove stale
   `-wal`/`-shm` if restoring a `.backup` output, which is already
   checkpointed).
3. Ensure ownership is `65532:65532`.
4. Start the server and run `observer-org migrate --config ...` — it reports
   the schema version and applies any pending migrations idempotently.

Agents keep working across a restore: their cursors are local, and re-pushes
are deduped on composite keys, so a restored-slightly-stale DB self-heals on
the next push cycle.

---

## 3. Key rotation

### Session HMAC key (`session.key`)

Lowest-stakes. Replace the file and restart. Effect: all dashboard sessions are
invalidated; users re-authenticate via SAML on next visit. No data impact.

### SCIM token (`scim-token`)

1. Generate a new token (`openssl rand -hex 32`).
2. Update the IdP's SCIM client with the new token.
3. Replace the file and restart.
   Sequence the IdP update close to the restart to minimise the window where
   provisioning calls `401`.

### SAML SP cert/key (`sp.crt` / `sp.key`)

1. Generate a new keypair.
2. Re-upload the SP metadata / cert to the IdP so it trusts the new signing
   cert.
3. Replace the files and restart.
   Until the IdP trusts the new cert, signed AuthnRequests are rejected — do
   this in a maintenance window.

### Bearer signing key (`bearer-signing.key`) — disruptive

This key signs **every** agent bearer. Rotating it invalidates all existing
bearers: every enrolled agent must **re-enrol** (`observer enroll` with a fresh
token). Rotate only on suspected compromise. Procedure: replace the key,
restart, then re-issue enrolment tokens and have developers re-enrol. Prefer
**revoking individual bearers** (below) over rotating the signing key.

### Revoking a single agent

From the dashboard (admin → bearers) or by recording the bearer's `jti` in
`revoked_bearers`. `VerifyBearer` checks the revocation list on every push, so
a revoked agent's next push gets `401`. This does not require rotating the
signing key or disturbing other agents.

---

## 4. Upgrades

Agent and server ship at the **same semver tag**; compatibility is "matching
minor". Upgrade the server first, then agents, within the same minor.

**v1.7.x → v1.8.x note.** The 2026-06-03 release introduced the new
privacy posture (hashes ship by default; raws gated behind a per-node
TOML opt-in). Both directions of the upgrade are forward-compatible:

- v1.7.x agent → v1.8.x server: ingest computes the missing
  `*_hash` columns server-side from the raw values the old agent ships
  (`internal/orgserver/ingest/ingest.go::hashOrComputed`). Rows land
  with both shapes populated and rollups continue to work. After all
  agents have upgraded, run `observer-org scrub-content --all --confirm`
  to purge the now-redundant raw columns (the hash counterparts are
  preserved; rollup queries that keyed on the raws are already migrated
  in v1.8.x).
- v1.8.x agent → v1.7.x server: the new `*_hash` JSON keys are
  additive and ignored by the older schema. The `omitempty` raw
  columns ship empty in metadata-only mode, which the v1.7.x server
  accepts. Rollups still work; the server just doesn't have the
  hashes to dedup against.

```bash
# Docker
docker pull ghcr.io/marmutapp/observer-org:v1.7.2
# verify the signature before rolling
cosign verify ghcr.io/marmutapp/observer-org:v1.7.2 \
  --certificate-identity-regexp 'https://github.com/marmutapp/superbased-observer-private/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker stop observer-org && docker rm observer-org && docker run ... :v1.7.2

# Helm
helm upgrade observer-org charts/observer-org -n observer-org \
  --reuse-values --set image.tag=v1.7.2
```

Migrations run automatically on `serve` startup (and can be applied explicitly
with `observer-org migrate`). **Back up the DB before any upgrade** that may
carry a migration. The Helm chart uses a `Recreate` rollout so the old pod
releases the `ReadWriteOnce` volume before the new one binds it.

Supply-chain artefacts attached to each release: per-binary CycloneDX SBOMs
(`observer.cdx.json` / `observer-org.cdx.json`), SLSA Level 3 provenance
(`multiple.intoto.jsonl`), and the cosign-signed image. To verify the provenance
of a downloaded binary, use **slsa-verifier v2.7.0 or newer** (older versions
fail with `unexpected tlog entry type … got dsse:0.0.1`) and pass the private
origin repo as the source — the build runs there:

```bash
slsa-verifier verify-artifact ./observer-org \
  --provenance-path multiple.intoto.jsonl \
  --source-uri github.com/marmutapp/superbased-observer-private
```

### Scrubbing pre-v1.8.0 leaked content (one-time)

If your server ran v1.7.x agents before the v1.8.0 cutover, the
`actions.target` / `source_file` / `project_root` / `git_remote` /
`token_usage.source_file` columns hold raw command bodies, assistant
prose, and filesystem paths from those earlier pushes. The v1.8.x
server logs a startup WARN naming each affected column. To purge:

```bash
# Dry-run first — prints per-column row counts and exits.
docker exec observer-org observer-org scrub-content \
  --config /etc/observer-org/config.toml --all

# Apply (writes a .scrubbed-<ts>.bak alongside the DB first).
docker exec observer-org observer-org scrub-content \
  --config /etc/observer-org/config.toml --all --confirm
```

The `*_hash` counterparts are preserved, so dedup keys + rollup
queries continue to work. Per-column flags (`--actions-target`,
`--sessions-git-remote`, etc.) let you scrub a subset instead of
`--all`. `--skip-backup` is available for installations with an
external backup pipeline.

---

## 5. Troubleshooting

### `observer-org doctor`

Run the built-in health check first — it validates config, DB access, secret
file presence/permissions, and reports the schema version:

```bash
docker exec observer-org observer-org doctor --config /etc/observer-org/config.toml
# or: kubectl exec deploy/observer-org -- observer-org doctor --config ...
```

### Server won't start

- **`fetch IdP metadata from ... failed`** — `saml.idp_metadata_url` is
  unreachable from the pod, or wrong. The server fetches it eagerly at startup.
  Fix the URL/network and restart.
- **`unable to open database file` / SQLITE_CANTOPEN** — the data volume is not
  writable by uid 65532. Ensure `/var/lib/observer-org` is owned `65532:65532`
  (Docker) or that the Helm `fsGroup: 65532` is in effect (it is by default).
- **Secret read errors** — the mounted secret files must be readable by uid
  65532; the SCIM token must be `0600` or `doctor` flags it.

### SAML login fails

- **Clock skew** — SAML assertions are time-bounded. If the IdP and server
  clocks differ by more than the tolerance, logins fail with an assertion
  validity error. Ensure NTP on both sides; skew is the most common cause.
- **Attribute mapping** — if the dashboard shows a user with no email/teams,
  the IdP isn't emitting the attributes named in `saml.attribute_mapping`.
  Inspect the assertion and align the mapping.
- **Audience/ACS mismatch** — the IdP's Audience must equal
  `saml.sp_entity_id` and its ACS must be `<external_url>/saml/acs`.

### SCIM returns 4xx

- **`401`** — wrong SCIM bearer token; the IdP's configured token doesn't match
  `scim-token`. Re-sync after a rotation.
- **`409` on create** — the user/group already exists (idempotent provisioning
  re-run); usually benign.
- **`404` on patch/delete** — the IdP references a resource id the server
  doesn't have (provisioning drift); a full re-sync from the IdP reconciles it.

### Agent pushes fail

- **`401` with a stale-timestamp error** — agent and server clocks differ by
  more than ±300s. Fix NTP on the agent host.
- **`401` after working before** — the bearer was revoked, or the bearer
  signing key was rotated. The developer must re-enrol with a fresh token.
- **persistent `5xx`/network** — the agent backs off and retries; it never
  blocks local use. Check `observer org status` for the last error and the
  server logs for the corresponding request id.

### Dashboard shows nothing

- You are not in `dashboard.admin_emails` and are not a team lead. Members see
  nothing by design. Add your email to `admin_emails` and restart, or have an
  admin assign you a lead role via SCIM group membership.

---

## 5b. Guard rollups, RBAC and dashboard policy authoring (v1.8.3 / G14)

Agents with the guard layer enabled push content-free guard-event rows
(rule id, category, severity, decision, enforced, hashes, chain links —
see guard spec §10.2); the server lands them in `guard_events` and the
dashboard's **Security** page rolls them up: verdict trends, rule-hit
leaderboards, per-team posture, and per-agent audit-chain continuity.
The chain report is a per-developer disclosure and is **audited** like
the cost drill-down (`view_guard_agents` rows in the audit log).

Two new `[dashboard]` config email lists layer the §14.5 roles onto the
existing admin/lead model (an admin implicitly holds both):

```toml
[dashboard]
admin_emails           = ["ops@acme.example"]
policy_admin_emails    = ["seceng@acme.example"]   # author/publish bundles + org-wide guard reads
security_viewer_emails = ["audit@acme.example"]    # org-wide guard reads, no policy write
```

Team leads see their teams' guard slice; plain members see nothing.

The **Policy** page shows the bundle version history and, for a
`policy_admin`, authors new versions through the same lint+sign+insert
gate as `observer-org policy publish` (each publish is audit-logged as
`publish_policy_bundle`). Dashboard publishing requires
`[policy].signing_key_path`; the server loads the key per publish and
retains nothing between requests. Operators who prefer the CLI-only
signing posture (the v1.8.3/G13 default) simply leave
`policy_admin_emails` empty — the authoring panel then refuses every
publish.

---

## 6. Data retention

`server.data_retention_days` (default 730) is the documented horizon for pushed
org data. Deprovisioning a user via SCIM removes their access immediately. Note:
this broad horizon is config-validated but **not yet enforced by a sweep** — it
documents intent, not an active pruner.

### Message-content retention (implemented, default OFF)

The one server-side prune that IS implemented is for stored message bodies
(`otel_content`, surfaced by the session message-content viewer). It mirrors the
individual node's retention model:

```toml
[dashboard.content_retention]
otel_content_days = 0   # 0 = keep forever (default). >0 = prune bodies older than N days.
```

When `otel_content_days > 0`, a daily sweep (`internal/orgserver/retention`)
**NULLs the `content` body** of any row past the horizon while **keeping its
`content_hash`** and the row itself — so the audit trail, dedup, and idempotent
re-push all survive; only the large body bytes are reclaimed. The sweep is
idempotent (a second run clears nothing new) and runs alongside the HTTP server,
like the analytics pollers. Large bodies are accepted by design; turn this on
only if you want a hard age cap on message content.

> Message bodies only ever reach the server from nodes that enabled native-OTel
> capture AND content sharing (`full_content` / `admin_managed`). Viewing them is
> an audited, deeper disclosure (`view_session_messages` audit_log rows). There
> is no remote toggle to force content collection on.
