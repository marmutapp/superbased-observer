-- 038 — split stale_reads into wasteful + necessary (spec §14.1)
--
-- The freshness pipeline already flags FreshnessStale reads on
-- actions.freshness (a target file changed between the prior read
-- and the current one). The cache-tracking arc lets us further
-- distinguish:
--
--   - WASTEFUL: the prior copy still sat inside a live cached
--     prefix at re-read time AND no compaction_reset / expiry
--     evicted it. The re-read was genuinely avoidable; the agent
--     could have used the cached copy.
--   - NECESSARY: a compaction_reset or expiry_rewrite intervened
--     between the prior read and this one. The cached copy is
--     gone; the re-read is the only way to get the content back.
--
-- Both nullable: sessions without cache data (Tier 3 / pre-
-- backfill / non-Anthropic providers) leave the new fields NULL
-- per spec §14.1 — "no fake zeros" on sessions cachetrack hasn't
-- seen.
--
-- redundancy_ratio_wasteful is the same fraction the existing
-- redundancy_ratio computes, but over the wasteful subset only.
-- The dashboard "Redundancy" surface renders both side by side
-- so operators see the genuinely-avoidable share.

ALTER TABLE sessions ADD COLUMN stale_reads_wasteful INTEGER;
ALTER TABLE sessions ADD COLUMN stale_reads_necessary INTEGER;
ALTER TABLE sessions ADD COLUMN redundancy_ratio_wasteful REAL;
