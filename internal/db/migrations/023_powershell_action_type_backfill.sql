-- 023_powershell_action_type_backfill.sql — backfill the v1.6.11
-- cross-adapter shell-name normalization (Issue #6,
-- docs/handovers/next-session-handover-2026-05-19.md).
--
-- Pre-fix: every adapter's actionMap recognized "Bash" / "bash" /
-- "shell" / "exec" etc., but PowerShell / pwsh / cmd / cmd.exe fell
-- through to ActionUnknown. Windows-side claude-code sessions
-- invoking powershell.exe, codex's exec_command tool, and the
-- copilot-cli powershell tool all silently dropped out of the
-- dashboard's run_command-filtered views (Actions tab type filter).
--
-- Codex's exec_command tool name (separate from "shell" / "shell_command")
-- was also never mapped — 260 maintainer-corpus rows land here.
--
-- This UPDATE re-derives action_type='run_command' for any historical
-- actions row whose raw_tool_name matches the now-recognized shell-
-- interpreter or codex exec set. Scoped by raw_tool_name + the
-- still-unknown action_type so the migration is idempotent — re-running
-- on a clean DB is a no-op (every match was already updated on the
-- first run).
--
-- Scoped per-adapter to mirror exactly what each adapter's actionMap
-- now accepts. The codex exec_command rule applies only to codex rows;
-- the shell-interpreter rule covers all adapters that route through
-- shell-interpreter tool names.

UPDATE actions
SET action_type = 'run_command'
WHERE action_type = 'unknown'
  AND (
    -- shell-interpreter tool names landing under any adapter:
    raw_tool_name IN (
      'PowerShell', 'powershell', 'Powershell',
      'pwsh', 'Pwsh',
      'cmd', 'Cmd', 'cmd.exe', 'CMD.EXE',
      'sh', 'Sh',
      'bash', 'Bash'
    )
    -- codex-only synonym: the modern Desktop tool name for shell exec.
    -- Bash is uppercase in claudecode/cowork and lowercase in
    -- copilot-cli — both already covered by the IN clause above.
    OR (tool = 'codex' AND raw_tool_name = 'exec_command')
  );
