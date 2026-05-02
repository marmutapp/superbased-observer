-- 003_compression_stats.sql — per-turn conversation compression savings
-- (spec §10 Layer 3 step 11). Columns are nullable because turns captured
-- before conversation compression shipped, and every turn the compressor
-- skipped, leave them unset. Aggregators treat NULL the same as zero.

ALTER TABLE api_turns ADD COLUMN compression_original_bytes   INTEGER;
ALTER TABLE api_turns ADD COLUMN compression_compressed_bytes INTEGER;
ALTER TABLE api_turns ADD COLUMN compression_count            INTEGER;
ALTER TABLE api_turns ADD COLUMN compression_dropped_count    INTEGER;
ALTER TABLE api_turns ADD COLUMN compression_marker_count     INTEGER;
