-- 004_policy_bundles.sql — org guard-policy bundle channel (guard spec
-- §14.2, G13).
--
-- One row per published bundle version: the §4.4-format TOML rule set,
-- its Ed25519 signature over orgcontract.PolicyBundleSigningMessage,
-- and the public half of the policy signing key used (stored per row
-- so key-rotation history stays auditable and the serve path returns
-- the matching key verbatim). version is AUTOINCREMENT so versions
-- stay strictly monotonic even across deletes — the agent's downgrade
-- protection (reject version < last verified) depends on that.
--
-- The latest version is the served bundle (GET /api/v1/policy-bundle);
-- older rows are the §14.2 version history. Publishing is server-side
-- only (observer-org policy publish; dashboard authoring joins with
-- G14's RBAC) — agents never write here, and nothing in this table
-- ever flows from agent to server.
CREATE TABLE IF NOT EXISTS org_policy_bundles (
    version     INTEGER PRIMARY KEY AUTOINCREMENT,
    bundle_toml TEXT NOT NULL,
    signature   TEXT NOT NULL,            -- base64url Ed25519 signature
    public_key  TEXT NOT NULL,            -- base64url Ed25519 public key (the signer's)
    signed_at   TEXT NOT NULL,            -- RFC3339
    created_by  TEXT NOT NULL DEFAULT '', -- operator identity (CLI: OS user; G14: SAML user)
    description TEXT NOT NULL DEFAULT ''  -- operator note for the version history
);
