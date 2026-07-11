-- 045_process_metrics.sql — per-process resource metrics for Process
-- Observability (docs/plans/process-obs-dashboard-enhancements-2026-06-17.md).
--
-- Adds current/peak compute-memory-disk counters to process_runs, captured by
-- the Windows poll capturer each poll (GetProcessTimes / GetProcessMemoryInfo /
-- GetProcessIoCounters / GetProcessHandleCount) and refreshed for the attributed
-- AI subtree via a metrics-update event. CPU (cpu_user_ms/cpu_system_ms), peak
-- working set (max_rss_bytes) and cumulative disk bytes (read_bytes/write_bytes)
-- already exist on process_runs (migration 044); this adds the rest.
--
-- Network metrics are deliberately NOT added — per-process network bytes need
-- ETW (docs/process-observability.md §5.2), deferred.
--
-- metric_samples_json is a CAPPED in-row JSON ring buffer of recent
-- {t,cpu,ws,rb,wb} samples (throttled ~15s/sample, last ~60 points) that drives
-- the per-process sparklines — no separate samples table, so process_runs stays
-- the single owner and the data is pruned with the run (retention).
--
-- Privacy posture UNCHANGED: process_runs is NODE-LOCAL (pinned in
-- tests/invariant/privacy_test.go); these are numeric counters, no new
-- content/secret surface, and no org-push exposure.

ALTER TABLE process_runs ADD COLUMN working_set_bytes  INTEGER;
ALTER TABLE process_runs ADD COLUMN thread_count       INTEGER;
ALTER TABLE process_runs ADD COLUMN handle_count       INTEGER;
ALTER TABLE process_runs ADD COLUMN read_ops           INTEGER;
ALTER TABLE process_runs ADD COLUMN write_ops          INTEGER;
ALTER TABLE process_runs ADD COLUMN metric_samples_json TEXT;
