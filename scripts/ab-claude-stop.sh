#!/usr/bin/env bash
# ab-claude-stop.sh — stop the A/B daemons started by
# ab-claude-start.sh. Safe to run when nothing's up; never touches
# the user's main daemon (it lives under a different config path).

set -euo pipefail

ROOT="${AB_CLAUDE_ROOT:-/tmp/ab-claude}"

for side in on off; do
  pidfile="$ROOT/$side/observer.pid"
  if [[ -f "$pidfile" ]]; then
    pid=$(cat "$pidfile")
    if kill -0 "$pid" 2>/dev/null; then
      echo "stopping $side daemon (pid $pid)" >&2
      kill "$pid" 2>/dev/null || true
      sleep 1
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
  fi
done

# Belt-and-suspenders: kill anything still bound to our two ports
# that matches the observer binary path. Avoids leaks from prior
# manual `bin/observer start --config ...` invocations.
for port in 8830 8831; do
  pids=$(lsof -ti :$port 2>/dev/null || true)
  for pid in $pids; do
    cmd=$(ps -p "$pid" -o comm= 2>/dev/null || true)
    if [[ "$cmd" == "observer" ]]; then
      echo "killing leftover observer on :$port (pid $pid)" >&2
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
done
