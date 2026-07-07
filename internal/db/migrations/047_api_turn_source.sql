-- Native-console integration (docs/native-console-integration-template.md,
-- Phase 2). Adds provenance to api_turns so a single turn observed by more than
-- one source (proxy / provider-native telemetry / JSONL) collapses into ONE row
-- keyed by request_id, with source-derived fidelity deciding precedence.
--
--   source = NULL or 'proxy'  -> proxy intercept   (FidelityProxyExact)
--   source = 'cc_otel'        -> Claude Code OTel   (FidelityNativeExact)
--   source = 'jsonl'          -> JSONL usage stream (FidelityApprox)
--
-- NULL is read as 'proxy': every api_turns row written before this migration
-- came from the proxy (the only writer of this table historically), so legacy
-- rows keep their rank without a backfill. The source string is node-local
-- provenance metadata (a short enum) and is NOT pushed to the org wire.
--
-- The store seam internal/store/merge.go::UpsertTurnByRequestID owns the
-- read-modify-write; internal/turnmerge owns the precedence. request_id is
-- intentionally NOT made UNIQUE here — pre-existing rows may legitimately carry
-- duplicate/empty request_ids, and a blind UNIQUE index would fail to apply on
-- established installs. The index below is for the upsert's lookup path only.
ALTER TABLE api_turns ADD COLUMN source TEXT;

CREATE INDEX IF NOT EXISTS idx_api_turns_request_id
    ON api_turns(request_id) WHERE request_id IS NOT NULL;
