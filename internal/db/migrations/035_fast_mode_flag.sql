-- 035_fast_mode_flag.sql — v1.8.2 fast-mode pricing premium.
--
-- Anthropic Opus 4.8 ships a low-latency "fast" tier (request param
-- speed:"fast") that bills every per-token dimension at 2× the standard
-- rate ($10/$50 vs $5/$25). The cost engine models this via
-- Pricing.FastMultiplier and TokenBundle.Fast (cost/engine.go,
-- cost/pricing.go); rows need a column to persist the per-turn flag so
-- the engine reconstructs accurate cost on hot-reload of the pricing
-- table.
--
-- Captured by the proxy when the outbound Anthropic Messages API
-- request carries "speed":"fast". Adapters that parse on-disk logs
-- with a fast/speed signal can set it too; otherwise default 0.
--
-- Node-local only: NOT added to the org-push wire shape. The agent
-- computes cost_usd at insert time with FastMultiplier already applied
-- (see internal/proxy + internal/store), and the wire ships pre-
-- computed dollar amounts. The server doesn't need the flag.
--
-- INTEGER NOT NULL DEFAULT 0 keeps the column SQLite-bool-typical
-- shape; pre-existing rows pass the constraint immediately.

ALTER TABLE api_turns   ADD COLUMN fast INTEGER NOT NULL DEFAULT 0;
ALTER TABLE token_usage ADD COLUMN fast INTEGER NOT NULL DEFAULT 0;
