-- 024_claudecode_action_type_backfill.sql — backfill the v1.6.11
-- Issue #6 extension: re-derive action_type for historical Claude Code
-- rows whose raw_tool_name is now mapped in the adapter's actionMap
-- but were ingested before the mapping landed.
--
-- Live maintainer DB (2026-05-19) had these Claude Code unknown rows
-- for tools that ARE mapped in adapter.go's actionMap today:
--
--   TaskUpdate   1817 → todo_update
--   TaskCreate    963 → todo_update
--   Agent         211 → spawn_subagent
--   ExitPlanMode   22 → permission_mode
--   TodoWrite      16 → todo_update
--   TaskList        9 → todo_update
--   EnterPlanMode   9 → permission_mode
--   TaskGet/TaskOutput/TaskStop/AskUserQuestion — handful each
--
-- Total ≈ 3,047 rows that should have proper categories. Pre-fix
-- these all landed under the "Other / unknown" bucket in the dashboard
-- Actions tab type filter, hiding the agent's planning + subagent
-- fan-out + plan-mode toggles from per-category aggregations.
--
-- NOT included (left as ActionUnknown because no good action_type
-- category exists yet):
--   Monitor, ScheduleWakeup, ToolSearch, mcp__*  — adding categories
--   for these is a separate taxonomy decision; meanwhile they stay
--   unknown so the operator can spot them in the existing "Other"
--   bucket and decide whether to extend.
--
-- Idempotent — re-running on a clean DB is a no-op (matches only
-- WHERE action_type='unknown', which the prior run already cleared).
-- Tool-scoped to 'claude-code' so a future adapter that happens to
-- emit "TodoWrite" as its own tool name doesn't get accidentally
-- retagged.

UPDATE actions
SET action_type = 'todo_update'
WHERE tool = 'claude-code'
  AND action_type = 'unknown'
  AND raw_tool_name IN (
    'TaskCreate', 'TaskUpdate', 'TaskList',
    'TaskGet', 'TaskOutput', 'TaskStop',
    'TodoWrite'
  );

UPDATE actions
SET action_type = 'spawn_subagent'
WHERE tool = 'claude-code'
  AND action_type = 'unknown'
  AND raw_tool_name = 'Agent';

UPDATE actions
SET action_type = 'ask_user'
WHERE tool = 'claude-code'
  AND action_type = 'unknown'
  AND raw_tool_name = 'AskUserQuestion';

UPDATE actions
SET action_type = 'permission_mode'
WHERE tool = 'claude-code'
  AND action_type = 'unknown'
  AND raw_tool_name IN ('EnterPlanMode', 'ExitPlanMode');
