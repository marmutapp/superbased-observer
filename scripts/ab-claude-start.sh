#!/usr/bin/env bash
# ab-claude-start.sh — bring up both A/B observer daemons in the
# background. Idempotent — re-running stops any prior daemons before
# starting fresh.
#
# PIDs land in /tmp/ab-claude/{on,off}/observer.pid; logs in
# /tmp/ab-claude/{on,off}/observer.log.

set -euo pipefail

ROOT="${AB_CLAUDE_ROOT:-/tmp/ab-claude}"
BIN="${OBSERVER_BIN:-./bin/observer}"

if [[ ! -x "$BIN" ]]; then
  echo "$BIN not found — run 'make build' first" >&2
  exit 1
fi
if [[ ! -d "$ROOT" ]]; then
  echo "$ROOT not found — run scripts/ab-claude-setup.sh first" >&2
  exit 1
fi

# Stop any prior A/B daemons. Don't touch the user's main daemon
# (different config path / different DB).
"$(dirname "$0")/ab-claude-stop.sh" || true

start_one() {
  local side=$1
  local cfg="$ROOT/$side/observer-config.toml"
  local pidfile="$ROOT/$side/observer.pid"
  local logfile="$ROOT/$side/observer.log"
  if [[ ! -f "$cfg" ]]; then
    echo "missing $cfg — run scripts/ab-claude-setup.sh" >&2
    return 1
  fi
  echo "starting $side daemon ($cfg)" >&2
  nohup "$BIN" start --config "$cfg" --no-dashboard \
    > "$logfile" 2>&1 &
  echo $! > "$pidfile"
  disown || true
}

start_one on
start_one off

# Wait a moment then verify both proxy ports are bound. Connect-only
# probe via /dev/tcp — sending a real HTTP request would land as a
# 404 api_turn in the DB and pollute the A/B baseline.
sleep 3
ok=1
for spec in 'on:8830' 'off:8831'; do
  side=${spec%:*}
  port=${spec##*:}
  if (echo > /dev/tcp/127.0.0.1/$port) >/dev/null 2>&1; then
    echo "$side daemon up on :$port ✓" >&2
  else
    echo "$side daemon NOT listening on :$port — see $ROOT/$side/observer.log" >&2
    ok=0
  fi
done

if [[ $ok -ne 1 ]]; then
  exit 1
fi

# Pre-flight: actually exercise the proxy with a /v1/messages POST.
# A successful pre-flight returns Anthropic's "invalid x-api-key" 401
# (we send a fake key on purpose) — proves the request reached the
# proxy and the proxy forwarded to api.anthropic.com. Anything else
# (connection-refused, 404, gateway timeout) means the daemon is up
# but not handling /v1/* — and a Claude Code session pointed at it
# would silently bypass and go direct to Anthropic.
preflight() {
  local side=$1
  local port=$2
  local body
  body=$(curl -sS --max-time 5 -o - -w '\nHTTP_STATUS=%{http_code}' \
    -H 'x-api-key: ab-claude-preflight' \
    -H 'anthropic-version: 2023-06-01' \
    -H 'content-type: application/json' \
    -d '{"model":"claude-sonnet-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}' \
    "http://127.0.0.1:$port/v1/messages" 2>&1 || echo 'CURL_FAILED')
  if echo "$body" | grep -q 'invalid x-api-key'; then
    echo "$side preflight ✓  (proxy forwarded to api.anthropic.com — got 401 on fake key)" >&2
    return 0
  fi
  echo "$side preflight ✗  unexpected response on :$port — proxy may not be capturing /v1/messages" >&2
  echo "$body" | sed 's/^/    /' >&2
  return 1
}
preflight on  8830 || true
preflight off 8831 || true

# Stamp the run start so ab-claude-report.sh can filter to this run's
# rows only — defends the headline from contamination by smoke-test
# rows or prior runs left in the same DB. UTC ISO 8601 with millisecond
# precision matches the timestamp format the proxy writes into
# api_turns. Slightly back-dated (-2s) so the proxy's own start-time
# pre-flight rows fall on the right side of the cutoff if they race.
date -u -d '2 seconds ago' +%Y-%m-%dT%H:%M:%S.%3NZ > "$ROOT/.run-started-at" 2>/dev/null \
  || date -u +%Y-%m-%dT%H:%M:%SZ > "$ROOT/.run-started-at"

cat <<EOF

Both daemons up. Next:

  Terminal A (compression ON):
    cd /tmp/ab-claude/on/repo
    observer claude --proxy http://127.0.0.1:8830

  Terminal B (compression OFF):
    cd /tmp/ab-claude/off/repo
    observer claude --proxy http://127.0.0.1:8831

The launcher (observer claude) sets ANTHROPIC_BASE_URL and — for
Pro/Max OAuth users — re-exports the access token from
~/.claude/.credentials.json as ANTHROPIC_AUTH_TOKEN. Without that,
OAuth-authenticated Claude Code 2.1+ bypasses ANTHROPIC_BASE_URL for
the /v1/messages chat call and the proxies capture nothing. Same
Pro/Max billing on the wire — observer just gets to see (and
compress) the body.

Run the same 4 prompts in each, then:
  ./scripts/ab-claude-report.sh
EOF
