-- 013_api_turn_errors.sql
--
-- Add error capture columns to api_turns. Pre-fix the proxy
-- explicitly dropped non-2xx upstream responses (`if resp.StatusCode <
-- 200 || resp.StatusCode >= 300 { return }` in proxy.go) so failed
-- requests left no trace in observer. Companion to the v1.4.20
-- claudecode adapter fix that captures the same failures from the
-- JSONL side.
--
-- The proxy now records errored turns with zero token counts and
-- these three fields populated:
--
--   * http_status   — upstream HTTP response code (e.g. 400, 429, 529)
--   * error_class   — upstream error classification
--                     (`invalid_request_error` / `rate_limit_error` /
--                     `overloaded_error` / `permission_error` / etc.)
--                     Parsed from the standard Anthropic / OpenAI
--                     error envelope. Defaults to NULL when the body
--                     wasn't JSON or the envelope shape was unknown.
--   * error_message — human-readable message after secrets scrubbing.
--                     Truncated to action_excerpts.MaxExcerptBytes
--                     before insert so a misbehaving upstream can't
--                     blow up the row size.
--
-- Successful turns (status 2xx) leave all three NULL — the existing
-- token-bearing api_turns rows are unchanged. Per-turn dedup against
-- token_usage continues to match by request_id ↔ source_event_id; a
-- failed turn that the JSONL adapter also captured (as ActionAPIError)
-- naturally joins on api_turns.request_id = actions.target.
--
-- No backfill: pre-fix errors were never visible to the proxy so we
-- have no historical rows to populate. Going forward both surfaces
-- record their own view of the same failure.

ALTER TABLE api_turns ADD COLUMN http_status   INTEGER;
ALTER TABLE api_turns ADD COLUMN error_class   TEXT;
ALTER TABLE api_turns ADD COLUMN error_message TEXT;

CREATE INDEX IF NOT EXISTS idx_api_turns_http_status ON api_turns(http_status) WHERE http_status IS NOT NULL;
