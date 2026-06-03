-- 008_cache_creation_1h.sql — split 5m vs 1h ephemeral cache_creation
-- tracking. Anthropic's prompt-caching API now exposes
-- usage.cache_creation.{ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}.
-- The 1h tier is priced at 2× the default 5m tier; folding the two into a
-- single cache_creation_input_tokens column under-counts cost when the
-- caller mixes tiers (always under, since the 1h rate is the higher one).
--
-- Strategy: keep cache_creation_tokens as the *total* (so historical rows
-- and any caller that doesn't set the breakdown stay coherent), and add
-- cache_creation_1h_tokens as the subset that landed in the 1h tier. The
-- cost engine subtracts to get the 5m portion. NULL/0 in the new column
-- means "all 5m" — correct for every row written before this migration,
-- since 1h-tier traffic was zero in the live install at that point.

ALTER TABLE api_turns ADD COLUMN cache_creation_1h_tokens INTEGER;
ALTER TABLE token_usage ADD COLUMN cache_creation_1h_tokens INTEGER;
