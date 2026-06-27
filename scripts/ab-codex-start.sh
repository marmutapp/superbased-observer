#!/usr/bin/env bash
# ab-codex-start.sh — bring up both codex A/B observer daemons in
# the background. Idempotent — re-running stops any prior daemons
# before starting fresh.
#
# PIDs land in /tmp/ab-codex/{on,off}/observer.pid; logs in
# /tmp/ab-codex/{on,off}/observer.log.

set -euo pipefail

ROOT="${AB_CODEX_ROOT:-/tmp/ab-codex}"
BIN="${OBSERVER_BIN:-./bin/observer}"

if [[ ! -x "$BIN" ]]; then
  echo "$BIN not found — run 'make build' first" >&2
  exit 1
fi
if [[ ! -d "$ROOT" ]]; then
  echo "$ROOT not found — run scripts/ab-codex-setup.sh first" >&2
  exit 1
fi

"$(dirname "$0")/ab-codex-stop.sh" || true

start_one() {
  local side=$1
  local cfg="$ROOT/$side/observer-config.toml"
  local pidfile="$ROOT/$side/observer.pid"
  local logfile="$ROOT/$side/observer.log"
  if [[ ! -f "$cfg" ]]; then
    echo "missing $cfg — run scripts/ab-codex-setup.sh" >&2
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

sleep 3
ok=1
for spec in 'on:8832' 'off:8833'; do
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

# Pre-flight: hit /v1/responses with a fake API key. Expected: a 401
# from api.openai.com — proves the proxy forwarded. Anything else
# means the daemon is up but not handling the OpenAI Responses path,
# and a codex session pointed at it would silently bypass.
preflight() {
  local side=$1
  local port=$2
  local body
  body=$(curl -sS --max-time 5 -o - -w '\nHTTP_STATUS=%{http_code}' \
    -H 'Authorization: Bearer sk-ab-codex-preflight-fake' \
    -H 'content-type: application/json' \
    -d '{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}' \
    "http://127.0.0.1:$port/v1/responses" 2>&1 || echo 'CURL_FAILED')
  if echo "$body" | grep -qE 'HTTP_STATUS=401|invalid api key|Incorrect API key'; then
    echo "$side preflight ✓  (proxy forwarded to api.openai.com — got 401 on fake key)" >&2
    return 0
  fi
  echo "$side preflight ✗  unexpected response on :$port — proxy may not be capturing /v1/responses" >&2
  echo "$body" | sed 's/^/    /' >&2
  return 1
}
preflight on  8832 || true
preflight off 8833 || true

date -u -d '2 seconds ago' +%Y-%m-%dT%H:%M:%S.%3NZ > "$ROOT/.run-started-at" 2>/dev/null \
  || date -u +%Y-%m-%dT%H:%M:%SZ > "$ROOT/.run-started-at"

cat <<EOF

Both codex daemons up. Next:

  Terminal A (compression ON):
    cd $ROOT/on/repo
    observer codex --proxy http://127.0.0.1:8832

  Terminal B (compression OFF):
    cd $ROOT/off/repo
    observer codex --proxy http://127.0.0.1:8833

The launcher (observer codex) injects -c openai_base_url='"<proxy>/v1"'
into codex's argv so the Responses API request lands at the proxy.
Both auth shapes (sk- API key + Bearer eyJ JWT for ChatGPT-Plus) flow
through the same override — the proxy detects the bearer shape and
path-translates to chatgpt.com when needed.

Run the same 4 prompts in each, then:
  ./scripts/ab-codex-report.sh
EOF
