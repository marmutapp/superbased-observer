-- 0004_span_cost.sql — capture per-component cost detail + provenance on
-- obs_spans (Gap B, docs/plans/obs-capture-gap-audit-2026-06-28.md). The total
-- stays in the existing cost_usd column; these two add the breakdown and where
-- it came from:
--   cost_source — 'reported' (the instrumentor emitted cost, e.g. OpenInference
--                 llm.cost.*) or 'computed' (no cost reported → priced by the
--                 host cost engine through the injected SpanPricer). NULL when
--                 neither (no model/tokens).
--   cost_detail — JSON {input,output,cache_read,cache_write,reasoning,tool}
--                 holding only the components present; display-only, the hero/
--                 list aggregate still sums cost_usd.
-- Both nullable, authoritative-on-merge like the existing cost column. Node-
-- local (obs_*), no agent migration, no wire/privacy change.
ALTER TABLE obs_spans ADD COLUMN cost_source TEXT;
ALTER TABLE obs_spans ADD COLUMN cost_detail TEXT;
