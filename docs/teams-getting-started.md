# Teams & Org — Getting Started

This guide takes you from nothing to a running **observer-org** server with
SAML SSO, SCIM provisioning, and your first enrolled developer agent pushing
data. It is the install/onboarding companion to
[teams-architecture.md](teams-architecture.md) (how it works) and
[teams-operations.md](teams-operations.md) (running it day to day).

> **Solo users need none of this.** The `observer` agent works fully
> standalone. Org mode is purely additive: a developer's local experience is
> byte-identical whether or not they enrol. You only stand up `observer-org`
> when you want org-wide visibility.

---

## 0. Local in 5 minutes (v1.8.0+)

Use this flow to evaluate Teams locally on a single machine. It runs the
dev compose stack, lets you log in via a SAML-free dev bypass, mints an
enrolment link, and ships a metadata-only push from a freshly-built agent.
Production deployment is §3+.

**Prereqs:** Docker, Docker Compose, the repo cloned. Nothing else.

```bash
# 1. Build the binaries.
make build
PATH="$PWD/bin:$PATH"

# 2. Bring the org stack up + provision an admin + mint a token.
observer-org quickstart
#   ──> Dashboard:  http://localhost:8443
#       Dev-auth:   curl ... -d email=admin@example.com
#       Enrol cmd:  observer enroll http://localhost:8443 <token>

# 3. Enrol an agent (separate shell or the same).
observer enroll --link http://localhost:8443/enrol/<token>
#   ──> writes [org_client] block to ~/.observer/config.toml
#       with the v1.8.0 metadata-only defaults.

# 4. Run a brief AI session (claude / codex / cursor) and watch the
#    activity flow to the dashboard.
observer org status        # see what's queued
observer org push-now      # flush immediately, see exact row counts
observer org preview       # see the EXACT bytes that crossed the wire
```

**Privacy posture (v1.8.0+):** the default `[org_client.share].full_content
= false` means only hashes ship — your raw command bodies, assistant
prose, and filesystem paths stay local. The org admin **cannot flip
this** remotely; it lives in your node's config.toml. Set it to `true`
to ship raws (commands, prose, paths). `observer org preview` shows the
literal bytes you'd send before you commit.

**Dev-auth bypass:** the quickstart stack runs the org server with
`[server].dev_auth = true` and exposes `POST /auth/dev/login`, so you
log into the dashboard with an email instead of fighting SAML. The
server logs a WARN at startup when this is on; `/healthz` reports
`dev_auth: true` so a production monitoring agent catches a
misconfigured deploy.

---

## 1. What you are deploying

| Component | Who runs it | What it is |
|---|---|---|
| `observer` | Each developer | The existing agent, installed per the main README. Gains an `observer enroll` command. |
| `observer-org` | One admin, server-side | The org server: SAML SSO, SCIM, enrolment-token minting, signed-push ingest, and the org dashboard. Ships as a Docker image (`ghcr.io/marmutapp/observer-org`) and a Helm chart (`charts/observer-org/`). |

The server listens on **:8443** (plain HTTP — terminate TLS upstream at your
ingress/load balancer) and serves:

| Path | Purpose | Auth |
|---|---|---|
| `/saml/metadata`, `/saml/sso`, `/saml/acs`, `/saml/slo` | SAML SP | — |
| `/scim/v2/...` | SCIM 2.0 provisioning | SCIM bearer token |
| `/api/org/...` | Org rollup API | SAML session (role-scoped) |
| `/` | Org dashboard SPA | SAML session |
| `POST /api/org/enrolment-tokens` | Mint enrolment tokens | SAML session (admin) |

---

## 2. Prerequisites

- A SAML 2.0 IdP you administer (Okta, Microsoft Entra ID, or Google
  Workspace are covered below).
- A DNS name for the server (e.g. `observer-org.example.com`) and a way to
  terminate TLS in front of it.
- One of: Docker, or a Kubernetes cluster + Helm 3.
- A secret store for the server's keys (or generate them by hand, below).

---

## 3. Generate the server secrets

The server reads five secret files. Generate them once and keep them in your
secret manager — they are the root of trust for the deployment.

```bash
mkdir -p secrets/saml
# SAML SP signing keypair (self-signed RSA is fine; some IdPs want a real CA cert)
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout secrets/sp.key -out secrets/sp.crt -subj "/CN=observer-org"
# Ed25519 bearer signing key (PKCS#8 PEM) — signs every agent bearer token
openssl genpkey -algorithm ed25519 -out secrets/bearer-signing.key
# HMAC session key (32 random bytes) — signs dashboard session cookies
head -c 32 /dev/urandom > secrets/session.key
# SCIM bearer token — your IdP authenticates to the SCIM endpoint with this
openssl rand -hex 32 > secrets/scim-token
```

Keep every file `0600`. The server's `doctor` subcommand checks that the SCIM
token is not world-readable.

---

## 4. Install the server

### Option A — Docker

```bash
# Lay out the config + secrets the container expects.
mkdir -p /etc/observer-org/saml /etc/observer-org/scim /etc/observer-org/bearer
cp secrets/sp.crt secrets/sp.key       /etc/observer-org/saml/
cp secrets/scim-token                  /etc/observer-org/scim/token
cp secrets/bearer-signing.key          /etc/observer-org/bearer/signing.key
cp secrets/session.key                 /etc/observer-org/session.key
# config.toml — see deploy/observer-org/config.toml for a complete example.
$EDITOR /etc/observer-org/config.toml

# The container runs as nonroot (uid 65532); the data dir and secret files
# must be readable/owned by it.
chown -R 65532:65532 /etc/observer-org
mkdir -p /var/lib/observer-org && chown 65532:65532 /var/lib/observer-org

docker run -d --name observer-org \
  -v /etc/observer-org:/etc/observer-org:ro \
  -v /var/lib/observer-org:/var/lib/observer-org \
  -p 8443:8443 \
  ghcr.io/marmutapp/observer-org:v1.7.0
```

The image is keyless-signed with cosign. Verify it before running in
production:

```bash
cosign verify ghcr.io/marmutapp/observer-org:v1.7.0 \
  --certificate-identity-regexp 'https://github.com/marmutapp/superbased-observer-private/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

For a self-contained local trial (server + a dev SAML IdP), use the compose
stack in `deploy/observer-org/` — its README walks through `keygen.sh` and
`docker compose up`.

### Option B — Kubernetes (Helm)

```bash
# 1. Create the Secret the chart references (keys, not paths).
kubectl create namespace observer-org
kubectl create secret generic observer-org-secrets -n observer-org \
  --from-file=bearer-signing.key=secrets/bearer-signing.key \
  --from-file=session.key=secrets/session.key \
  --from-file=sp.crt=secrets/sp.crt \
  --from-file=sp.key=secrets/sp.key \
  --from-file=scim-token=secrets/scim-token

# 2. Install. Override the example placeholders in values.yaml.
helm install observer-org charts/observer-org -n observer-org \
  --set secrets.existingSecret=observer-org-secrets \
  --set config.externalURL=https://observer-org.example.com \
  --set config.saml.spEntityID=https://observer-org.example.com/saml/metadata \
  --set config.saml.idpMetadataURL=https://your-idp/saml/metadata \
  --set 'config.dashboard.adminEmails={you@example.com}' \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set 'ingress.hosts[0].host=observer-org.example.com'
```

See `charts/observer-org/values.yaml` for every knob (persistence size,
resources, TLS, probes).

---

## 5. Configure SAML SSO

Fetch the SP metadata once the server is up:

```bash
curl -k https://observer-org.example.com/saml/metadata
```

Then register the SP in your IdP. The server expects three assertion
attributes — `email`, `displayName`, and `groups` — remapped via
`saml.attribute_mapping` in config if your IdP names them differently.

- **Okta** — *Applications → Create App Integration → SAML 2.0*. Single
  sign-on URL = `https://observer-org.example.com/saml/acs`, Audience =
  `https://observer-org.example.com/saml/metadata`. Add `email`,
  `displayName`, `groups` to the attribute statement. Put the Okta IdP
  metadata URL into `saml.idp_metadata_url`.
- **Microsoft Entra ID** — *Enterprise applications → New → Create your own*.
  Reply URL (ACS) = `.../saml/acs`, Identifier = `.../saml/metadata`. Map
  claims to `email`, `displayName`, `groups`. Use the *App Federation
  Metadata URL* for `idp_metadata_url`.
- **Google Workspace** — *Admin → Apps → Web and mobile apps → Add custom
  SAML app*. ACS URL = `.../saml/acs`, Entity ID = `.../saml/metadata`. Add
  attribute mappings for primary email and display name (Google does not emit
  groups by default — leave the `groups` mapping unused if so).

The server fetches IdP metadata **at startup** from `idp_metadata_url`, so it
must be reachable when the pod/container boots.

---

## 6. Configure SCIM provisioning

Point your IdP's SCIM client at:

```
Base URL:  https://observer-org.example.com/scim/v2
Token:     <the value of secrets/scim-token>
```

Provisioned **users** become org members; provisioned **groups** become teams.
A group member whose SCIM role marks them a lead becomes that team's lead.
Deprovisioning a user removes their access; the data they already pushed is
retained per `server.data_retention_days`.

---

## 7. First login and designate admins

Browse to `https://observer-org.example.com/` — you are redirected through
SAML. Anyone who is in `dashboard.admin_emails` sees the org dashboard; team
leads see their team; everyone else sees nothing. Set `admin_emails` in config
(or `config.dashboard.adminEmails` in Helm) to at least your own email before
first use, or no one can see the dashboard.

### What the dashboard surfaces

Once agents are enrolled and pushing, an admin sees (all content-free
aggregates, role-scoped):

- **Overview** — org spend with prior-period deltas, 4-bucket token totals,
  cache efficiency, reliability (proxy vs estimated) split, error rate, latency
  (proxy-only), tool/model mix, and activity rhythm.
- **People** — an org-wide per-developer leaderboard that works **without SCIM
  team groups** (it keys on the developer identity that already ships). The
  aggregate KPIs render immediately; the named per-developer table is an
  **audited disclosure** (a click writes a `view_org_developers` audit row),
  the same posture as the per-team developer drill-down.
- **Teams / Projects** — per-team and per-project (git-root) spend, with
  cross-team overlap indicators.
- **Tools / Models / Activity** — per-tool and per-model breakdowns, plus
  time-of-day and day-of-week activity grids.
- **Telemetry** (admin-only) — native-console vendor analytics from each
  provider's own org-analytics API (Claude Code / Codex / Copilot): vendor cost
  mix, Claude Code accept rate, Copilot seat utilization, engagement counts.
  This requires a separately-configured poller per vendor; until one is wired
  the page shows an honest "not configured" state.
- **Security / Policy / Audit** — guard-layer fleet posture, policy-bundle
  authoring/history, and the append-only audit log.

Metrics that depend on the proxy (latency, cache efficiency, per-turn HTTP
error class) only populate when the agent routed through `:8820`; without it
they degrade to a `token_usage` fallback or a labeled empty — token, cost,
action, tool, and model metrics survive on the watcher path alone.

---

## 8. Mint the first enrolment token

Enrolment tokens are one-time, short-lived credentials a developer exchanges
for a long-lived signed bearer. Mint one from the dashboard (admin → Invite,
or admin → Settings → Enrolment tokens — both flows back onto the same
endpoint; admins see a dropdown populated from the SCIM-provisioned user
list via `GET /api/org/members`, non-admins fall back to a free-text
`user_id` input) or from the server CLI:

```bash
# Docker:
docker exec observer-org observer-org new-enrolment-token \
  --config /etc/observer-org/config.toml --email dev@example.com
# Kubernetes:
kubectl exec -n observer-org deploy/observer-org -- \
  observer-org new-enrolment-token --config /etc/observer-org/config.toml --email dev@example.com
```

It prints a compound `<token_id>.<secret>` string. Hand it to the developer
over a secure channel — it is single-use and expires per
`enrolment.default_token_lifetime_days`.

---

## 9. Enrol a developer agent

On the developer's machine, with `observer` already installed and running:

```bash
observer enroll https://observer-org.example.com <token>
```

This generates an Ed25519 keypair locally, exchanges the token for a 90-day
bearer (bound to that public key), and stores both in the OS keychain (a
`0600` file fallback on headless boxes). The agent now pushes content-free
rollup rows on its normal cadence. Useful follow-ups:

```bash
observer org status      # enrolment state, last push, next push
observer org push-now    # force an immediate push
observer unenroll        # stop pushing and clear local org state
```

---

## 10. Verify data flows end to end

1. **Agent side** — `observer org status` shows "enrolled" and a recent
   successful push. `observer org push-now` returns accepted/deduped counts.
2. **What is shared** — the dashboard's Enrolment page (and the agent's stored
   last-payload) shows the exact content-free rows that were pushed. No
   prompts, command bodies, or file contents ever leave the machine.
3. **Org side** — as an admin, open the dashboard: the developer appears under
   their team, with spend and activity rolled up. Drilling into an individual
   developer is recorded in the audit log before the data is shown.

If a push fails, see the troubleshooting section of
[teams-operations.md](teams-operations.md) (clock skew, SCIM 4xx, bearer
revocation).

---

## Optional: OpenTelemetry export

Independently of org enrolment, each agent can export per-turn LLM spans to
your own OTel collector (`gen_ai.*` + `sbo.*` attributes). It is **off by
default**; enable `[exporter.otel]` in the agent config. See
[teams-architecture.md](teams-architecture.md#the-otel-exporter-second-rail)
and the reference dashboards under `docs/exporters/otel/`.
