-- 022_claudecode_bash_duration_cap.sql — backfill the v1.6.10 / audit
-- B5 bash-duration cap landed at claudecode/adapter.go:744-756.
--
-- The adapter infers per-tool wallclock from the gap between the
-- tool_use assistant-message timestamp and the paired tool_result
-- user-message timestamp. When auto-compact stitches an in-flight
-- tool_use to a tool_result that lands hours later, OR the user
-- resumes a session-idle conversation hours after the original
-- prompt, the inferred duration measures the IDLE GAP, not the
-- tool's actual execution time. Claude Code's Bash tool has a
-- documented 30-minute hard ceiling; any computed duration above
-- the cap is a capture artifact.
--
-- Prior audit (docs/claudecode-bash-duration-audit-2026-05-15.md):
-- 62 rows with duration_ms > 3,600,000 (1 hour) claiming a
-- cumulative 246.4 false hours of Bash wallclock across 20 sessions.
-- Top session (d7657cad) alone had 73.3 false hours.
--
-- This UPDATE zeros out duration_ms on any claude-code action row
-- whose computed duration exceeds the 30-minute cap. Idempotent:
-- re-running on a post-fix DB has no rows above the threshold to
-- update.
--
-- Scoping: tool = 'claude-code' AND duration_ms > 30*60*1000.
-- Native-tool rows (Read/Write/Edit/Grep/Glob) all run in sub-second
-- wallclock so the cap is forgiving; Bash is where the inflation
-- concentrates (per prior audit's source-file breakdown table).
--
-- Note: this UPDATE is rather than DELETE because the action row
-- itself is legitimate — the tool call did happen and we want it on
-- the timeline. Only the DurationMs attribution was wrong.

UPDATE actions
SET duration_ms = 0
WHERE tool = 'claude-code'
  AND duration_ms > 1800000;
