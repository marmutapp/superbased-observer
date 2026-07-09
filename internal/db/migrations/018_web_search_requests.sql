-- 018_web_search_requests.sql — track per-event web_search invocations
-- so Anthropic's "$10 per 1,000 searches" server-side-tool billing can be
-- applied in addition to per-token costs.
--
-- Anthropic charges this as a flat per-request fee outside the token
-- accounting. The token cost of any content retrieved by a web_search is
-- already in the input_tokens / cache_read_tokens of the next turn — what
-- this column captures is the SEPARATE per-search fee.
--
-- NULL / 0 means "no web_searches in this event" which is correct for
-- every row written before this migration (and for every event from
-- adapters that don't yet emit the field). The cost engine adds
-- web_search_requests × Pricing.WebSearchPerRequest to the total cost;
-- entries with WebSearchPerRequest=0 contribute nothing, so leaving the
-- field unset on non-Anthropic providers is also correct.
--
-- See docs/cowork-cost-drift-investigation-2026-05-15.md §"Phase 2" for
-- the per-session validation that closed the residual haiku-cost drift
-- on the cowork reconciliation tile.

ALTER TABLE token_usage ADD COLUMN web_search_requests INTEGER;
ALTER TABLE api_turns   ADD COLUMN web_search_requests INTEGER;
