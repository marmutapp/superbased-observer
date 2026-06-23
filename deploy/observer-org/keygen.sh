#!/bin/sh
# keygen.sh — generate dev secrets for the org-server compose stack into the
# shared /etc/observer-org volume, and copy the dev config.toml in. Idempotent:
# existing files are left untouched so restarts keep the same keys (and thus a
# stable org_id and valid sessions).
#
# DEV ONLY. These are throwaway, network-local secrets. Production secrets
# come from a secret manager, never from a generator baked into compose.
set -eu

CONF=/etc/observer-org
mkdir -p "$CONF/saml" "$CONF/bearer" "$CONF/scim"

if [ ! -f "$CONF/config.toml" ]; then
  cp /seed/config.toml "$CONF/config.toml"
fi

# SP signing keypair (RSA self-signed) for SAML AuthnRequest signing.
if [ ! -f "$CONF/saml/sp.key" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
    -keyout "$CONF/saml/sp.key" -out "$CONF/saml/sp.crt" \
    -subj "/CN=observer-org-dev" >/dev/null 2>&1
fi

# Ed25519 bearer signing key (PKCS#8 PEM).
if [ ! -f "$CONF/bearer/signing.key" ]; then
  openssl genpkey -algorithm ed25519 -out "$CONF/bearer/signing.key" >/dev/null 2>&1
fi

# HMAC session key (32 random bytes).
if [ ! -f "$CONF/session.key" ]; then
  head -c 32 /dev/urandom > "$CONF/session.key"
fi

# Static SCIM token (known dev value so the fake SCIM client can use it).
if [ ! -f "$CONF/scim/token" ]; then
  printf '%s' "dev-scim-token-change-me" > "$CONF/scim/token"
fi

# Lock down the SCIM token and keys (doctor checks 0600 on the SCIM token).
chmod 600 "$CONF/scim/token" "$CONF/session.key" "$CONF/bearer/signing.key" "$CONF/saml/sp.key"

# The org server runs as the distroless 'nonroot' user (uid/gid 65532) and
# mounts this volume read-only. Hand ownership of the seeded config + secrets
# to that uid so the server can read the 0600 keys; without this the read-only
# nonroot mount cannot open session.key / bearer / SCIM token / SAML key.
# DEV ONLY — production secrets are provisioned with correct ownership by the
# platform or secret manager, not by this generator.
chown -R 65532:65532 "$CONF"

echo "keygen: dev secrets ready in $CONF (owned by nonroot uid 65532)"
