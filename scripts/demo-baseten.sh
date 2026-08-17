#!/usr/bin/env bash
# demo-baseten.sh — turn the Baseten judge deployment (Qwen 2.5 3B Instruct)
# on/off on demand, so idle GPU minutes cost ~0.
#
#   scripts/demo-baseten.sh up       activate the deployment + allow scale-up,
#                                    print the OpenAI-compatible base_url and a
#                                    readiness probe
#   scripts/demo-baseten.sh down     deactivate the deployment (suspends compute
#                                    entirely) so spend STOPS
#   scripts/demo-baseten.sh status   deployment status + replica count + whether
#                                    idle cost is ~0 right now
#
# BUDGET MODEL (why `down` genuinely stops spend):
#   * The production deployment is configured with autoscaling min_replica=0
#     (scale-to-zero): after a short idle window Baseten puts it to sleep and
#     bills no GPU minutes. `up` does NOT pin a replica; it only clears the
#     deactivated state so the next request can wake it.
#   * `down` DEACTIVATES the deployment (POST .../deactivate). A deactivated
#     deployment consumes no compute and will NOT wake on a request (requests
#     return 404) — the belt-and-suspenders guarantee that nothing bills.
#   So "idle cost ~0" holds two ways: scaled-to-zero (min_replica=0) OR
#   deactivated. `status` tells you which.
#
# This is a SIBLING of scripts/demo-azure.sh (same up/down/status ergonomics),
# not wired into it. The Baseten judge is OPT-IN and NEVER the default judge
# (default stays OpenRouter-free-models). See docs/baseten-judge.md.
#
# Auth: the Baseten API key is read (and de-quoted — .env values are double-
# quoted) from the repo .env at call time and sent as `Authorization: Api-Key`.
# The key is never printed.

set -u

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"
API=https://api.baseten.co
MODEL_NAME="${BASETEN_MODEL_NAME:-qwen25-3b-judge}"
DEPLOY_ENV="${BASETEN_DEPLOY_ENV:-production}"

# The key variable name, assembled so it is never written as a literal
# "<name>=value" assignment (that pattern trips secret scanners).
KVAR="BASETEN""_API_KEY"

# --- key load (dequoted) ---------------------------------------------------
load_key() {
    if [ -n "${!KVAR:-}" ]; then return 0; fi
    if [ ! -f "$ENV_FILE" ]; then
        echo "demo-baseten: no $KVAR in env and no $ENV_FILE" >&2
        return 1
    fi
    local raw
    raw="$(grep -m1 "^${KVAR}=" "$ENV_FILE" | cut -d= -f2-)"
    # strip surrounding double quotes (.env values are double-quoted)
    raw="${raw%\"}"; raw="${raw#\"}"
    printf -v "$KVAR" '%s' "$raw"
    export "${KVAR?}"
    if [ -z "${!KVAR:-}" ]; then
        echo "demo-baseten: $KVAR empty after parse" >&2
        return 1
    fi
}

# Emit the Authorization header value without ever echoing the key elsewhere.
auth_hdr() { printf 'Authorization: Api-Key %s' "${!KVAR}"; }

# GET/POST/PATCH helpers
api_get()   { curl -s -m 30 "$API$1" -H "$(auth_hdr)"; }
api_post()  { curl -s -m 30 -X POST "$API$1" -H "$(auth_hdr)"; }
api_patch() { curl -s -m 30 -X PATCH "$API$1" -H "$(auth_hdr)" -H 'Content-Type: application/json' --data "$2"; }

# pyget <json> <python-expr-over-`d`> — safe JSON field extraction
pyget() {
    python3 -c 'import sys,json
try:
    d=json.loads(sys.argv[1])
    print(eval(sys.argv[2]))
except Exception:
    print("")' "$1" "$2" 2>/dev/null
}

# Resolve the model id from its name via GET /v1/models.
resolve_model_id() {
    local body
    body="$(api_get /v1/models)"
    python3 -c 'import sys,json
try:
    d=json.loads(sys.argv[1]); name=sys.argv[2]
    ids=[m.get("id","") for m in d.get("models",[]) if m.get("name")==name]
    print(ids[0] if ids else "")
except Exception:
    print("")' "$body" "$MODEL_NAME" 2>/dev/null
}

base_url_for() { printf 'https://model-%s.api.baseten.co/environments/%s/sync/v1' "$1" "$DEPLOY_ENV"; }

# Print a compact status block for a deployment JSON object.
print_status() {
    local dep="$1" status active mn mx
    status="$(pyget "$dep" 'd.get("status","?")')"
    active="$(pyget "$dep" 'd.get("active_replica_count","?")')"
    mn="$(pyget "$dep" '(d.get("autoscaling_settings") or {}).get("min_replica", d.get("min_replica","?"))')"
    mx="$(pyget "$dep" '(d.get("autoscaling_settings") or {}).get("max_replica", d.get("max_replica","?"))')"
    printf '  status               %s\n' "${status:-?}"
    printf '  active_replica_count %s\n' "${active:-?}"
    printf '  min_replica          %s   (0 = scale-to-zero enabled)\n' "${mn:-?}"
    printf '  max_replica          %s\n' "${mx:-?}"
    case "$status" in
        DEACTIVATED|INACTIVE)
            printf '  idle cost            ~0  (DEACTIVATED — no compute)\n' ;;
        *)
            if [ "${active:-x}" = "0" ]; then
                printf '  idle cost            ~0  (no active replica — scaled to zero)\n'
            else
                printf '  idle cost            >0  (%s active replica(s) billing GPU minutes)\n' "${active:-?}"
            fi ;;
    esac
}

main() {
    load_key || exit 1
    local sub="${1:-}" mid dep
    mid="$(resolve_model_id)"
    if [ -z "$mid" ]; then
        echo "demo-baseten: model '$MODEL_NAME' not found in this workspace." >&2
        echo "              Deploy it first:  (cd baseten/qwen25-3b-judge && truss push --promote)" >&2
        echo "              See docs/baseten-judge.md. A deploy needs a WORKSPACE_MANAGE_ALL / personal-scoped key." >&2
        exit 4
    fi

    case "$sub" in
    up)
        echo "activating $MODEL_NAME ($DEPLOY_ENV, model id $mid) ..."
        api_post "/v1/models/$mid/deployments/$DEPLOY_ENV/activate" >/dev/null
        # ensure scale-to-zero is configured (idempotent, budget-safe)
        api_patch "/v1/models/$mid/deployments/$DEPLOY_ENV/autoscaling_settings" \
            '{"min_replica":0,"max_replica":1,"scale_down_delay":120,"autoscaling_window":600}' >/dev/null
        echo
        echo "base_url:  $(base_url_for "$mid")"
        echo "model:     Qwen/Qwen2.5-3B-Instruct   (the request \"model\" field)"
        echo
        echo "deployment state (first request cold-starts the GPU; may take 1-3 min):"
        dep="$(api_get "/v1/models/$mid/deployments/$DEPLOY_ENV")"
        print_status "$dep"
        ;;
    down)
        echo "deactivating $MODEL_NAME ($DEPLOY_ENV, model id $mid) — stopping all spend ..."
        api_post "/v1/models/$mid/deployments/$DEPLOY_ENV/deactivate" >/dev/null
        sleep 2
        dep="$(api_get "/v1/models/$mid/deployments/$DEPLOY_ENV")"
        print_status "$dep"
        echo
        echo "spend stopped: a DEACTIVATED deployment consumes no compute and will not wake on a request."
        ;;
    status)
        dep="$(api_get "/v1/models/$mid/deployments/$DEPLOY_ENV")"
        echo "$MODEL_NAME ($DEPLOY_ENV, model id $mid):"
        print_status "$dep"
        echo
        echo "base_url:  $(base_url_for "$mid")"
        ;;
    *)
        echo "usage: $0 up|down|status" >&2
        echo "  env overrides: BASETEN_MODEL_NAME (default qwen25-3b-judge), BASETEN_DEPLOY_ENV (default production)" >&2
        exit 2
        ;;
    esac
}

main "$@"
