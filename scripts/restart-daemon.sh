#!/usr/bin/env bash
# restart-daemon.sh — safely restart the Observer daemon when AI clients
# route their API traffic through the proxy (ANTHROPIC_BASE_URL → :PORT).
#
# WHY THIS EXISTS (2026-06-20 burn): the daemon IS the proxy. If you kill
# it while a live Claude Code / Codex session points ANTHROPIC_BASE_URL /
# codex base_url at :PORT, that session's next API call dies with
# ConnectionRefused — including the very session running the restart.
# The safe order, per the operator's rule, is:
#
#     route OFF  →  restart daemon  →  route ON
#
# This script does the whole thing as ONE atomic run (sub-second proxy
# downtime), so no inter-turn API call lands in the down-window, and it
# flips the claude-code route off first as belt-and-suspenders.
#
# Usage:
#   scripts/restart-daemon.sh                       # plain safe restart
#   scripts/restart-daemon.sh --compression on      # enable conversation compression, then restart
#   scripts/restart-daemon.sh --compression off     # disable it, then restart
#   scripts/restart-daemon.sh --no-route-toggle     # don't touch settings.json (rely on atomic relaunch only)
#   scripts/restart-daemon.sh --port 8820
#
# Idempotent and conservative: backs up every file it edits, never uses
# `pgrep observer` (that self-matches the caller's argv), finds the daemon
# strictly by the listening :PORT socket, and restores the route even on
# failure paths.
set -euo pipefail

PORT=8820
COMPRESSION=""          # "", "on", or "off"
ROUTE_TOGGLE=1
SETTINGS="${HOME}/.claude/settings.json"
CONFIG="${OBSERVER_CONFIG:-${HOME}/.observer/config.toml}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${REPO_ROOT}/bin/observer"
LOG="/tmp/obs.log"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --compression) COMPRESSION="${2:-}"; shift 2 ;;
    --no-route-toggle) ROUTE_TOGGLE=0; shift ;;
    --port) PORT="${2:-8820}"; shift 2 ;;
    --bin) BIN="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log() { printf '[restart-daemon] %s\n' "$*"; }

daemon_pid() { ss -ltnp 2>/dev/null | grep ":${PORT} " | grep -oP 'pid=\K[0-9]+' | head -1; }
port_up()    { ss -ltn 2>/dev/null | grep -q ":${PORT} "; }

[[ -x "$BIN" ]] || { echo "ERROR: observer binary not found/executable: $BIN (run 'make build')" >&2; exit 1; }

# --- 1. optional compression config flip (no effect until the restart) ---
if [[ -n "$COMPRESSION" ]]; then
  case "$COMPRESSION" in on) want=true ;; off) want=false ;; *) echo "ERROR: --compression must be on|off" >&2; exit 2 ;; esac
  [[ -f "$CONFIG" ]] || { echo "ERROR: config not found: $CONFIG" >&2; exit 1; }
  cp "$CONFIG" "${CONFIG}.bak.restart"
  # Flip ONLY the [compression.conversation].enabled key — i.e. the first
  # `enabled =` line after that header and before the next subsection.
  python3 - "$CONFIG" "$want" <<'PY'
import re, sys
path, want = sys.argv[1], sys.argv[2]
lines = open(path).read().splitlines(keepends=True)
out, in_sec, done = [], False, False
for ln in lines:
    s = ln.strip()
    if s.startswith('[compression.conversation]'):
        in_sec = True
    elif s.startswith('[') and s != '[compression.conversation]':
        in_sec = False
    if in_sec and not done and re.match(r'enabled\s*=', s):
        ln = re.sub(r'(enabled\s*=\s*)(true|false)', r'\g<1>'+want, ln)
        done = True
    out.append(ln)
open(path, 'w').write(''.join(out))
print('conversation.enabled ->', want, '(changed)' if done else '(KEY NOT FOUND)')
PY
  log "compression set to '$COMPRESSION' in $CONFIG (backup: ${CONFIG}.bak.restart)"
fi

# --- 2. route OFF (save current claude-code ANTHROPIC_BASE_URL, remove it) ---
SAVED_ROUTE=""
restore_route() {
  [[ "$ROUTE_TOGGLE" -eq 1 ]] || return 0
  [[ -n "$SAVED_ROUTE" ]] || { log "no prior route to restore (was unset)"; return 0; }
  python3 - "$SETTINGS" "$SAVED_ROUTE" <<'PY'
import json, sys
path, url = sys.argv[1], sys.argv[2]
d = json.load(open(path)); d.setdefault('env', {})['ANTHROPIC_BASE_URL'] = url
json.dump(d, open(path, 'w'), indent=2)
PY
  log "route RESTORED: ANTHROPIC_BASE_URL=$SAVED_ROUTE"
}
trap 'restore_route' EXIT

if [[ "$ROUTE_TOGGLE" -eq 1 && -f "$SETTINGS" ]]; then
  cp "$SETTINGS" "${SETTINGS}.bak.restart"
  SAVED_ROUTE="$(python3 - "$SETTINGS" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); print(d.get('env', {}).get('ANTHROPIC_BASE_URL', ''))
PY
)"
  if [[ -n "$SAVED_ROUTE" ]]; then
    python3 - "$SETTINGS" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); d.get('env', {}).pop('ANTHROPIC_BASE_URL', None)
json.dump(d, open(sys.argv[1], 'w'), indent=2)
PY
    log "route OFF: removed ANTHROPIC_BASE_URL (was $SAVED_ROUTE; backup ${SETTINGS}.bak.restart)"
  else
    log "route already unset in settings.json"
  fi
fi

# --- 3. stop daemon (graceful, by :PORT socket only) ---
PID="$(daemon_pid || true)"
if [[ -n "$PID" ]]; then
  log "stopping daemon pid=$PID on :$PORT (SIGTERM)"
  kill -TERM "$PID" 2>/dev/null || true
  for _ in $(seq 1 40); do port_up || break; sleep 0.25; done
  if port_up; then log "WARN: :$PORT still listening after 10s; aborting before relaunch"; exit 1; fi
  log "daemon stopped"
else
  log "no daemon listening on :$PORT (will just start one)"
fi

# --- 3b. checkpoint WAL while down (best-effort) ---
DB="$(dirname "$CONFIG")/observer.db"
if command -v sqlite3 >/dev/null && [[ -f "$DB" ]]; then
  sqlite3 "$DB" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null 2>&1 || true
  log "WAL checkpointed ($DB)"
fi

# --- 4. relaunch + verify up ---
# Readiness window is generous (45s): a cold start runs pending DB
# migrations + upstream prewarms before the proxy listener binds, which can
# push past a tighter window and trip a false ERROR (observed 2026-06-29 on
# the migration-053 + prewarm start).
log "relaunching: $BIN start --no-open  (log: $LOG)"
setsid nohup "$BIN" start --no-open >"$LOG" 2>&1 </dev/null &
for _ in $(seq 1 180); do port_up && break; sleep 0.25; done
if ! port_up; then
  log "ERROR: daemon did not come up on :$PORT within 45s — see $LOG"
  tail -n 20 "$LOG" || true
  exit 1
fi
NEWPID="$(daemon_pid || true)"
log "daemon UP on :$PORT (pid=$NEWPID)"

# --- 5. route restored by the EXIT trap ---
log "done. proxy down-window was the stop→up gap above (typically <1s)."
