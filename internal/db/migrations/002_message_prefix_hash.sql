-- 002_message_prefix_hash.sql — adds message_prefix_hash to api_turns so the
-- proxy can record the stable prefix identifier used for cache-alignment
-- analysis (spec §10 Layer 3 step 5). The column is nullable because
-- pre-compression turns and turns without an observable prefix leave it
-- empty; the cost engine treats empty hashes the same as system_prompt_hash
-- absence.

ALTER TABLE api_turns ADD COLUMN message_prefix_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_api_turns_prefix_hash ON api_turns(message_prefix_hash);
