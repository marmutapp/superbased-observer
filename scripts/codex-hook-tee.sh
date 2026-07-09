#!/usr/bin/env bash
# Tee-shim for Codex hook payloads. Captures the JSON Codex sends on
# stdin to a timestamped file, then pipes the unchanged bytes through
# to the real observer binary. Use this to settle which envelope shape
# Codex emits on hook payloads (the speculative `effort.level` vs the
# JSONL-verified `collaboration_mode.settings.reasoning_effort`).
#
# Wire-up:
#   1. Back up ~/.codex/hooks.json
#   2. Replace each "command" entry with this shim path + the same
#      args, e.g.
#        "command": "/repo/scripts/codex-hook-tee.sh hook codex SessionStart --config '/home/u/.observer/config.toml'"
#   3. Re-trust the hooks in codex /hooks (commands changed, so codex
#      will prompt for trust again).
#   4. Run codex for ~30s of activity — even one prompt fires
#      SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop.
#   5. Inspect captures under $OBSERVER_CODEX_CAPTURE_DIR (default
#      /tmp/codex-hook-captures/).
#   6. Restore ~/.codex/hooks.json and re-trust.
#
# Config via env (override with `OBSERVER_BIN=... ./codex-hook-tee.sh`):
#   OBSERVER_BIN              path to observer binary
#   OBSERVER_CODEX_CAPTURE_DIR  where to write captures
#
# See docs/codex-hook-capture.md for the full procedure + decision rules.

set -euo pipefail

CAPTURE_DIR="${OBSERVER_CODEX_CAPTURE_DIR:-/tmp/codex-hook-captures}"
OBSERVER_BIN="${OBSERVER_BIN:-/home/marmutapp/superbased-observer/bin/observer}"

mkdir -p "$CAPTURE_DIR"

# Argv is the original codex hook command with the binary path
# replaced by this shim: `hook codex <Event> [--config ...]`. So the
# event name is the third positional argument.
EVENT="${3:-unknown}"
TS=$(date +%Y%m%d-%H%M%S-%N)
CAPTURE="$CAPTURE_DIR/${EVENT}-${TS}.json"

# `tee` writes to capture file + stdout; pipe stdout to observer so
# the hook's normal reply path (stdout JSON decision) is preserved.
# Codex reads observer's stdout — bytes from this script go through
# unchanged.
exec tee "$CAPTURE" | "$OBSERVER_BIN" "$@"
