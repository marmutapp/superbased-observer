#!/usr/bin/env bash
# demo-traffic.sh — synthesize hosted-app END-USER traffic through the demo
# gateway lane so the org dashboard's Plane-A surfaces (Trajectories,
# end-user spend, sessions, cost) show a lively multi-user picture.
#
#   scripts/demo-traffic.sh [rounds]      default 3 rounds
#
# Each round: every persona sends one prompt in a persistent per-persona
# session (multi-turn sessions look right in the explorer). Identity rides
# the same headers Open WebUI forwards (X-OpenWebUI-User-Email); session
# identity rides X-Superbased-Session. Free OpenRouter model only; the key
# comes from ~/.observer-org-demo/pi-openrouter.key (never inline).
# Traffic appears on the org dashboard within one push cycle (~60s).

set -u

GW="http://sbgateway35022.eastus.azurecontainer.io:8820/up/openrouter/v1/chat/completions"
MODEL="nvidia/nemotron-3.5-lightning:free"
KEY_FILE="$HOME/.observer-org-demo/pi-openrouter.key"
ROUNDS="${1:-3}"

PERSONAS=(
  "priya@customer-one.example|billing"
  "tom@customer-one.example|onboarding"
  "lena@customer-two.example|api-integration"
  "sam@customer-two.example|analytics"
)

PROMPTS=(
  "Summarize the difference between a webhook and a polling integration in two sentences."
  "Draft a one-line status update for a delayed shipment."
  "What are three common causes of failed card payments?"
  "Explain rate limiting to a non-technical customer in one paragraph."
  "Suggest a subject line for a renewal reminder email."
  "What does idempotency mean for a payments API? Keep it short."
)

if [ ! -f "$KEY_FILE" ]; then
  echo "key file missing: $KEY_FILE" >&2
  exit 1
fi
KEY=$(tr -d ' \n' < "$KEY_FILE")
RUN_ID=$(date +%H%M%S)

for round in $(seq 1 "$ROUNDS"); do
  for p in "${PERSONAS[@]}"; do
    email="${p%%|*}"
    topic="${p##*|}"
    session="demo-${topic}-${RUN_ID}"
    prompt="${PROMPTS[$((RANDOM % ${#PROMPTS[@]}))]}"
    code=$(curl -s -o /dev/null -m 60 -w '%{http_code}' -X POST "$GW" \
      -H "Authorization: Bearer $KEY" \
      -H "Content-Type: application/json" \
      -H "X-OpenWebUI-User-Email: $email" \
      -H "X-Superbased-Session: $session" \
      -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":$(printf '%s' "$prompt" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}],\"max_tokens\":120}")
    echo "round $round  $email  session=$session -> $code"
    sleep 3   # stay polite to the free-tier model
  done
done
echo "done — expect the sessions on the org dashboard within ~60s (gateway push cycle)."
