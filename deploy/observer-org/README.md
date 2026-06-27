# observer-org — local dev stack & deployment notes

The `observer-org` server is the **customer-self-hosted** org-visibility
backend (built from `cmd/observer-org`). It is *not* the agent (`cmd/observer`)
— this directory only concerns the server image and its dev compose stack.

`docker-compose.yaml` brings up a throwaway, network-local stack:

| Service | Role |
|---|---|
| `keygen`  | one-shot: generates dev secrets (SAML SP keypair, Ed25519 bearer key, HMAC session key, a known SCIM token) into the `org-config` volume and seeds `config.toml`. Idempotent. |
| `idp`     | SimpleSAMLphp test SAML IdP (`kristophjunge/test-saml-idp`), healthcheck-gated. **Dev only.** |
| `org`     | the org server, built from `Dockerfile.observer-org` (distroless, runs as **nonroot** uid 65532). |
| `scim-provision` | optional `--profile tools` one-shot that provisions one user via SCIM. |

> **DEV ONLY.** Plain HTTP, generated throwaway secrets, a known SCIM token
> (`dev-scim-token-change-me`). Production uses HTTPS (TLS terminated
> upstream), real IdP metadata, and secrets from a secret manager.

## Run it

```bash
docker compose -f deploy/observer-org/docker-compose.yaml up -d --build
```

- Dashboard: <http://localhost:8443/> — 302-redirects to SSO when logged out.
- IdP login: `user1` / `user1pass` (test-saml-idp defaults).
- The IdP admin UI is also exposed on <http://localhost:8088/>.

### Smoke-test the server is actually up

`GET /saml/metadata` is public and only responds once the server has fully
started (DB opened, all secrets loaded, IdP metadata fetched):

```bash
curl -i http://localhost:8443/saml/metadata          # → 200 application/samlmetadata+xml
curl -i http://localhost:8443/                        # → 302 → /saml/sso
curl -i http://localhost:8443/scim/v2/Users           # → 401 (no token)
curl -i -H 'Authorization: Bearer dev-scim-token-change-me' \
        http://localhost:8443/scim/v2/Users           # → 200 SCIM ListResponse
```

## Container runs as nonroot — ownership matters

The `org` image is `distroless/static-debian12:nonroot`, so the process runs
as **uid/gid 65532**, not root. Two consequences are baked into this stack;
**do not regress them** or the server dies at startup:

1. **Data volume ownership.** `Dockerfile.observer-org` seeds an empty
   `/var/lib/observer-org` owned by `65532:65532` *before* the `VOLUME`
   instruction. A fresh named/anonymous volume inherits that directory's
   ownership, so the server can create `server.db`. Without it you get
   `orgserver/db.Open: ping: unable to open database file` (SQLITE_CANTOPEN).
   (distroless has no shell, so the dir is created in the build stage and
   `COPY --chown`'d in — a `RUN mkdir` in the runtime stage is impossible.)

2. **Secret ownership.** `keygen.sh` `chown -R 65532:65532`s the seeded config
   so the server (which mounts `/etc/observer-org` read-only) can read the
   `0600` keys (`session.key`, `bearer/signing.key`, `saml/sp.key`,
   `scim/token`). Without it, reading those root-owned `0600` files fails.

In production these are non-issues: the platform/secret-manager provisions
secrets with the correct ownership, and the data dir is a host path or PVC
owned by the runtime uid.

## Dev-box prerequisites (rootless Docker, no sudo)

`docker compose up` needs a working Docker daemon. On a Linux / WSL2 dev box
without root, use **rootless Docker**. The one package you cannot avoid
installing with sudo is `uidmap`:

```bash
sudo apt-get install -y uidmap          # provides newuidmap / newgidmap (setuid)
```

Everything else can run as your user:

```bash
export XDG_RUNTIME_DIR=/tmp/dockerd-rootless-xdg && mkdir -p "$XDG_RUNTIME_DIR"
dockerd-rootless-setuptool.sh check     # should print "Requirements are satisfied"
nohup dockerd-rootless.sh > /tmp/dockerd-rootless.log 2>&1 &
export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/docker.sock
docker info                             # rootless, storage-driver=overlay2
```

Notes for WSL2 specifically:

- **IPv6 / ip6tables warnings in the daemon log are benign** — rootless
  networking is IPv4 (slirp4netns); IPv6 NAT is unavailable.
- **DNS may time out on image pull** (`lookup registry-1.docker.io on
  10.0.2.3:53: i/o timeout`). slirp4netns's built-in resolver (`10.0.2.3`)
  does not always forward to the WSL NAT gateway. Point the daemon's
  *namespace* `/etc/resolv.conf` at a public resolver reachable through
  slirp's outbound NAT:

  ```bash
  dpid=$(pgrep -u "$(id -u)" -x dockerd | head -1)
  nsenter --preserve-credentials -t "$dpid" -U -m -n -- \
      sh -c 'printf "nameserver 8.8.8.8\nnameserver 1.1.1.1\n" > /etc/resolv.conf'
  ```

  (`uidmap` lets you `nsenter` into your own rootless namespace with full caps
  inside it.) A reusable alternative is a `~/.config/docker/daemon.json` /
  rootlesskit config, but the runtime override above is enough for a one-off.

If you have root, plain rootful Docker Desktop / `dockerd` needs none of this.

## CI

`ci.yml` does **not** run this stack today (it runs vet + `go test -race` +
build + the frontend gates). Running the compose stack in CI is a **Teams M5**
task; the ownership invariants above are what that job will exercise.
