#!/usr/bin/env bash
# run.sh — blocked-pair benchmark runner.
#
# Two live-safe modes:
#   --dry-run   parse arms.toml, validate the referenced corpus tasks +
#               assertions, enumerate blocks×pair×arm, print a
#               NO-NETWORK / NO-SPEND banner. Executes nothing.
#   --pilot     Phase-0a pilot: ONE paired block over the representative
#               tasks (both arms, order randomized within each pair). This
#               is the FIRST path that spends money; it is gated on a FROZEN
#               pre-registration (§3.0) and a MANDATORY hard budget cap
#               (--budget-usd). It never restarts/reconfigures the daemon —
#               per-arm compression is isolated via a per-workspace
#               `.observer/config.toml` overlay (config.ProjectCompression,
#               resolved by the proxy from the session CWD).
#
# Any other non-dry-run invocation clears the Phase-0a gate guard and dies
# (the full pre-registered run is powered from the pilot report first).
#
# Usage:
#   run.sh --dry-run [--arms <arms.toml>]
#   run.sh --pilot --budget-usd 15 [--model sonnet] [--arms F] [--db P]
#          [--observer-bin P] [--runs-dir D] [--session-timeout S]
#          [--per-session-budget-usd N] [--settle-secs S] [--date YYYY-MM-DD]

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"

ARMS="$HERE/arms.toml"
MODE=""
BUDGET_USD=""
MODEL="sonnet"
DB="$HOME/.observer/observer.db"
OBS_BIN=""
RUNS_DIR="$(bench_root)/runs"
SESSION_TIMEOUT="900"
PER_SESSION_BUDGET="3.0"
SETTLE_SECS="8"
RUN_DATE="$(date -u +%Y-%m-%d)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) MODE="dry" ;;
    --pilot)   MODE="pilot" ;;
    --arms) shift; ARMS="$1" ;;
    --budget-usd) shift; BUDGET_USD="$1" ;;
    --model) shift; MODEL="$1" ;;
    --db) shift; DB="$1" ;;
    --observer-bin) shift; OBS_BIN="$1" ;;
    --runs-dir) shift; RUNS_DIR="$1" ;;
    --session-timeout) shift; SESSION_TIMEOUT="$1" ;;
    --per-session-budget-usd) shift; PER_SESSION_BUDGET="$1" ;;
    --settle-secs) shift; SETTLE_SECS="$1" ;;
    --date) shift; RUN_DATE="$1" ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//; /^set -euo/d'
      exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
  shift
done

[[ -f "$ARMS" ]] || die "arms config not found: $ARMS"
[[ -n "$OBS_BIN" ]] || OBS_BIN="$(repo_root)/bin/observer"

# ----------------------------------------------------------------------
# validate_protocol — shared by --dry-run and --pilot. Reads arms.toml,
# checks every referenced representative task has a [success] gate + a
# semantic assertion + firing_independent=true, and checks every
# claude-code arm declares ENABLE_TOOL_SEARCH=true (the §R7 rig
# invariant). Populates the globals TASKS / ARMLINES / stratum / etc.
# ----------------------------------------------------------------------
validate_protocol() {
  log "== validating protocol =="
  mpie="$(toml_get "$ARMS" mpie_pct)"
  power="$(toml_get "$ARMS" power)"
  blocks="$(toml_get "$ARMS" blocks)"
  regime="$(toml_get "$ARMS" cache_regime)"
  prereg="$(toml_get "$ARMS" prereg)"
  stratum="$(toml_get "$ARMS" stratum)"

  [[ -n "$mpie" ]]   || die "protocol: mpie_pct missing (§3.0)"
  [[ -n "$power" ]]  || die "protocol: power missing (§3.0)"
  [[ -n "$regime" ]] || die "protocol: cache_regime missing (§3.3)"
  case "$regime" in cold|warm|mid-session) ;; *) die "protocol: cache_regime '$regime' invalid";; esac

  if [[ -n "$prereg" && -f "$(bench_root)/$prereg" ]]; then
    log "prereg: found $(bench_root)/$prereg"
  else
    warn "prereg '$prereg' not present — required before a LIVE run (§3.0)"
  fi

  [[ -n "$stratum" ]] || die "corpus: stratum missing (§3.7)"
  mapfile -t TASKS < <(toml_array "$ARMS" tasks)
  [[ ${#TASKS[@]} -gt 0 ]] || die "corpus: no tasks listed"
  for t in "${TASKS[@]}"; do
    td="$(bench_root)/corpus/$stratum/$t"
    [[ -f "$td/task.toml" ]] || die "task '$t': $td/task.toml missing"
    grep -Eq '^\[success\]' "$td/task.toml" || die "task '$t': no [success] gate (§3.7)"
    grep -Eq '^\[\[assertions\]\]' "$td/task.toml" || die "task '$t': no semantic assertion (§3.6)"
    if [[ "$stratum" == "representative" ]]; then
      fi_="$(toml_get "$td/task.toml" firing_independent)"
      [[ "$fi_" == "true" ]] || die "task '$t': representative tasks must be firing_independent=true (§3.7/R12)"
    fi
    log "task ok: $t"
  done

  mapfile -t ARMLINES < <(list_arms "$ARMS")
  [[ ${#ARMLINES[@]} -ge 2 ]] || die "need >= 2 arms (a matched pair); got ${#ARMLINES[@]}"
  for line in "${ARMLINES[@]}"; do
    IFS='|' read -r aid tool comp <<<"$line"
    [[ -n "$aid" && -n "$tool" ]] || die "arm '$line': id/tool required"
    if [[ "$tool" == "claude-code" ]]; then
      grep -A6 "id = \"$aid\"" "$ARMS" | grep -q 'ENABLE_TOOL_SEARCH=true' \
        || die "arm '$aid' (claude-code): ENABLE_TOOL_SEARCH=true not declared (§R7)"
    fi
    log "arm ok: $aid tool=$tool compression=${comp:-?}"
  done
}

# ======================================================================
# DRY-RUN
# ======================================================================
if [[ "$MODE" == "dry" ]]; then
  validate_protocol
  n_arms=${#ARMLINES[@]}
  log ""
  log "== execution plan (NOT executed) =="
  log "stratum:      $stratum"
  log "tasks:        ${TASKS[*]}"
  log "arms/pair:    $n_arms   blocks: ${blocks:-0}"
  log "mpie_pct:     $mpie   power: $power   regime: $regime"
  if [[ "${blocks:-0}" -le 0 ]]; then
    log "NOTE: blocks = ${blocks:-0} -> NOT POWERED (run --pilot to compute it)."
  fi
  total=$(( ${#TASKS[@]} * n_arms * ( blocks > 0 ? blocks : 0 ) ))
  log "would-run agent sessions (blocks×tasks×arms): $total"
  log ""
  log "########################################################"
  log "# NO NETWORK · NO API CALLS · NO SPEND were made.      #"
  log "########################################################"
  exit 0
fi

# ======================================================================
# Anything that is neither --dry-run nor --pilot hits the gate guard.
# ======================================================================
if [[ "$MODE" != "pilot" ]]; then
  assert_phase0a_gate "$ARMS"
fi

# ======================================================================
# PILOT — the first path that spends money.
# ======================================================================
# Hard gates first.
[[ -n "$BUDGET_USD" ]] || die "--pilot requires --budget-usd <cap> (mandatory budget enforcement)"
prereg_path="$(bench_root)/$(toml_get "$ARMS" prereg)"
[[ -f "$prereg_path" ]] || die "pilot: pre-registration not found: $prereg_path"
grep -qiE '^> \*\*FROZEN' "$prereg_path" || die "pilot: pre-registration is NOT frozen (header must say FROZEN) — freeze §12 first (§3.0)"
[[ -x "$OBS_BIN" ]] || die "pilot: observer binary not executable: $OBS_BIN"
[[ -f "$DB" ]] || die "pilot: observer DB not found: $DB"
command -v claude >/dev/null 2>&1 || die "pilot: claude binary not on PATH"
command -v jq >/dev/null 2>&1 || die "pilot: jq required"

validate_protocol

CARD="tool-defs-trim"
mkdir -p "$RUNS_DIR"
LEDGER="$RUNS_DIR/${CARD}-${RUN_DATE}.jsonl"
RUN_MANIFEST="$RUNS_DIR/${CARD}-${RUN_DATE}.manifest.json"
RUNROOT="$(mktemp -d "/tmp/observer-bench-${RUN_DATE}.XXXXXX")"

# Sibling drift manifest (§3.4 / ledger.md rule).
"$HERE/manifest.sh" --observer-bin "$OBS_BIN" > "$RUN_MANIFEST"

log ""
log "== Phase-0a PILOT: $CARD =="
log "budget cap:   \$$BUDGET_USD (hard)   per-session cap: \$$PER_SESSION_BUDGET"
log "model:        $MODEL"
log "observer bin: $OBS_BIN"
log "db:           $DB"
log "ledger:       $LEDGER"
log "manifest:     $RUN_MANIFEST"
log "workspaces:   $RUNROOT"
log "prereg:       FROZEN ✓"
log ""

: > "$LEDGER"   # fresh ledger for this pilot
CUMULATIVE="0"
BLOCK=1
ABORTED=0

# jnum/jstr — emit a JSON field value (number or quoted string), null-safe.
jnum() { local v="${1:-}"; [[ -z "$v" || "$v" == "null" ]] && printf 'null' || printf '%s' "$v"; }

# --- per-task field extraction (multi-line prompt) -------------------
task_prompt() {  # awk: capture [prompt].text triple-quoted block
  awk '
    /^\[prompt\]/ { inp=1; next }
    inp && /^\[/ && !intext { inp=0 }
    inp && /text[[:space:]]*=[[:space:]]*"""/ { intext=1; next }
    intext && /"""/ { intext=0; inp=0; next }
    intext { print }
  ' "$1"
}
task_success_cmd() { toml_get "$1" command; }
task_assert_script() { toml_get "$1" script; }

# fixed-length (64 hex) semantically-inert salt content, unique per
# (card,block,arm) but EQUAL SIZE across arms within a pair (§3.2).
salt_of() { printf '%s' "${CARD}|b${BLOCK}|$1" | sha256sum | awk '{print $1}'; }

# within-pair order: deterministic on block-index (+task), seed_source
# = block-index per arms.toml. 2-arm pair → A,B or B,A.
pair_order() {  # $1=task ; echoes two arm ids in run order
  local task="$1" a b h
  a="$(printf '%s\n' "${ARMLINES[@]}" | awk -F'|' '$1 ~ /^A-/{print $1; exit}')"
  b="$(printf '%s\n' "${ARMLINES[@]}" | awk -F'|' '$1 ~ /^B-/{print $1; exit}')"
  h=$(( 0x$(printf 'b%s|%s' "$BLOCK" "$task" | sha256sum | cut -c1-8) % 2 ))
  if [[ "$h" -eq 0 ]]; then echo "$a $b"; else echo "$b $a"; fi
}

# write the per-arm compression overlay into a workspace root.
write_overlay() {  # $1=workspace-root $2=arm-id
  local ws="$1" aid="$2"
  mkdir -p "$ws/.observer"
  if [[ "$aid" == A-* ]]; then
    cat > "$ws/.observer/config.toml" <<'OVL'
# Benchmark arm A-control — tool-defs-trim pilot.
# Conversation compression OFF for this workspace's traffic: full tool
# definitions sent every turn (the frozen control arm). A project overlay
# can only turn conversation compression OFF, never on (the daemon master
# is enabled=true), so this takes effect without touching the daemon.
[compression.conversation]
enabled = false
OVL
  else
    cat > "$ws/.observer/config.toml" <<'OVL'
# Benchmark arm B-toolsdefs-trim — tool-defs-trim pilot.
# ONLY the "tools" sentinel (tool-definitions trim). All other per-type
# compressors (json/logs/code/text) and the stash are OFF, so the ONLY
# difference from arm A is the tool-defs trim itself. mode/target_ratio/
# preserve_last_n inherit the production claude-code recipe.
[compression.conversation]
enabled = true
compress_types = ["tools"]
[compression.conversation.stash]
enabled = false
OVL
  fi
}

# run one arm session; append a ledger row; update CUMULATIVE.
run_session() {
  local task="$1" aid="$2" order_idx="$3"
  local td="$(bench_root)/corpus/$stratum/$task"
  local ws="$RUNROOT/${task}__${aid}"
  local run_id="${CARD}-${RUN_DATE}-b0${BLOCK}-${aid}-${task}"
  rm -rf "$ws"; mkdir -p "$ws"
  cp -a "$td/workspace/." "$ws/"
  write_overlay "$ws" "$aid"
  # git init so claude's tools behave normally + we can diff.
  ( cd "$ws" && git init -q && git add -A && git -c user.email=b@b -c user.name=b commit -qm base ) >/dev/null 2>&1 || true

  local prompt salt full_prompt succ_cmd assert_script
  prompt="$(task_prompt "$td/task.toml")"
  salt="$(salt_of "$aid")"
  full_prompt="${prompt}"$'\n\n'"<test-id>${salt}</test-id>"
  succ_cmd="$(task_success_cmd "$td/task.toml")"
  assert_script="$td/$(task_assert_script "$td/task.toml")"

  # budget pre-flight: refuse to START a session that could breach the cap.
  local proj; proj=$(python3 -c "print(f'{float(\"$CUMULATIVE\")+float(\"$PER_SESSION_BUDGET\"):.4f}')")
  if python3 -c "import sys; sys.exit(0 if float('$proj')>float('$BUDGET_USD') else 1)"; then
    warn "BUDGET PRE-FLIGHT: cumulative \$$CUMULATIVE + per-session cap \$$PER_SESSION_BUDGET would exceed \$$BUDGET_USD — aborting before $run_id"
    printf '{"run_id":"%s","card":"%s","block":%d,"arm":"%s","task_id":"%s","stratum":"%s","status":"aborted","excluded":true,"exclusion_reason":"budget_cap_preflight","cumulative_before_usd":%s}\n' \
      "$run_id" "$CARD" "$BLOCK" "$aid" "$task" "$stratum" "$CUMULATIVE" >> "$LEDGER"
    ABORTED=1
    return 0
  fi

  local pre_max_id started ended rc sid sdk_cost sdk_turns is_err
  pre_max_id="$(sqlite3 "file:$DB?mode=ro" -readonly 'SELECT COALESCE(MAX(id),0) FROM api_turns;')"
  started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  log "  → [$run_id] launching (cwd=$ws)"
  set +e
  ( cd "$ws" && ENABLE_TOOL_SEARCH=true timeout "${SESSION_TIMEOUT}s" \
      "$OBS_BIN" claude -- \
        -p "$full_prompt" \
        --model "$MODEL" \
        --dangerously-skip-permissions \
        --max-budget-usd "$PER_SESSION_BUDGET" \
        --output-format json \
    ) > "$ws/.agent-out.json" 2> "$ws/.agent-err.log"
  rc=$?
  set -e
  ended="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  # parse claude json result (single-object). Fields may be absent on
  # a hard failure / timeout → default null.
  sid=""; sdk_cost="null"; sdk_turns="null"; is_err="null"
  if [[ -s "$ws/.agent-out.json" ]] && jq -e . "$ws/.agent-out.json" >/dev/null 2>&1; then
    sid="$(jq -r '.session_id // empty' "$ws/.agent-out.json")"
    sdk_cost="$(jq -r '.total_cost_usd // .cost_usd // "null"' "$ws/.agent-out.json")"
    sdk_turns="$(jq -r '.num_turns // "null"' "$ws/.agent-out.json")"
    is_err="$(jq -r 'if .is_error==true then "true" elif .is_error==false then "false" else "null" end' "$ws/.agent-out.json")"
  fi

  local status excl reason
  status="ok"; excl="false"; reason="null"
  if [[ $rc -eq 124 ]]; then status="failed"; excl="true"; reason='"harness_confound_timeout"';
  elif [[ $rc -ne 0 ]]; then status="failed"; excl="false"; reason="null"; fi

  # settle: let the proxy's detached insert land, then extract for THIS sid.
  local ev='{}'
  if [[ -n "$sid" ]]; then
    sleep "$SETTLE_SECS"
    # poll until row-count stabilises (max ~40s)
    local c1 c2 tries=0
    c1="$(sqlite3 "file:$DB?mode=ro" -readonly "SELECT COUNT(*) FROM api_turns WHERE session_id='$sid' AND id>$pre_max_id;")"
    while (( tries < 8 )); do
      sleep 4
      c2="$(sqlite3 "file:$DB?mode=ro" -readonly "SELECT COUNT(*) FROM api_turns WHERE session_id='$sid' AND id>$pre_max_id;")"
      [[ "$c1" == "$c2" && "$c1" != "0" ]] && break
      c1="$c2"; tries=$((tries+1))
    done
    ev="$("$HERE/extract.sh" --db "$DB" --session "$sid" --min-id "$pre_max_id")"
  fi
  local turns cost cc uncached cread cwrite cwrite1h out model
  turns="$(jq -r '.turns // 0' <<<"$ev")"
  cost="$(jq -r '.est_list_price_usd // 0' <<<"$ev")"
  cc="$(jq -r '.compression_count // 0' <<<"$ev")"
  uncached="$(jq -r '.uncached_input // 0' <<<"$ev")"
  cread="$(jq -r '.cache_read // 0' <<<"$ev")"
  cwrite="$(jq -r '.cache_write // 0' <<<"$ev")"
  cwrite1h="$(jq -r '.cache_write_1h // 0' <<<"$ev")"
  out="$(jq -r '.output // 0' <<<"$ev")"
  model="$(jq -r '.model // ""' <<<"$ev")"

  # capture failure: session ran but no api_turns row landed → flag.
  if [[ "$status" == "ok" && ( -z "$sid" || "$turns" == "0" ) ]]; then
    status="failed"; excl="true"; reason='"capture_no_api_turns"'
  fi

  # quality guard: success gate + semantic assertion + diff stat.
  local succ_rc="null" assert_pass="null" files_changed="null"
  if [[ -n "$succ_cmd" ]]; then
    ( cd "$ws" && bash -c "$succ_cmd" ) >/dev/null 2>&1 && succ_rc=0 || succ_rc=$?
  fi
  if [[ -f "$assert_script" ]]; then
    if ( bash "$assert_script" "$ws" ) >/dev/null 2>&1; then assert_pass="true"; else assert_pass="false"; fi
  fi
  files_changed="$( ( cd "$ws" && git diff --name-only HEAD 2>/dev/null | wc -l | tr -d ' ' ) || echo null )"

  # activation: fired = any tool-defs/compression event on this session.
  local fired="false"; [[ "${cc:-0}" != "0" && "${cc:-0}" != "null" ]] && fired="true"

  # ledger row (retained even on failure/exclusion — ledger.md rule).
  {
    printf '{'
    printf '"run_id":"%s","card":"%s","block":%d,"pair_index":%s,"arm":"%s","tool":"claude-code","task_id":"%s","stratum":"%s",' \
      "$run_id" "$CARD" "$BLOCK" "$order_idx" "$aid" "$task" "$stratum"
    printf '"started_at":"%s","ended_at":"%s","cache_regime":"warm","salt_bytes":64,"session_id":"%s","agent_exit":%d,"manifest_ref":"%s",' \
      "$started" "$ended" "$sid" "$rc" "$(basename "$RUN_MANIFEST")"
    printf '"status":"%s","excluded":%s,"exclusion_reason":%s,' "$status" "$excl" "$reason"
    printf '"endpoint_primary":{"est_list_price_usd":%s,"turns":%s},' "$(jnum "$cost")" "$(jnum "$turns")"
    printf '"cache_vector":{"uncached_input":%s,"cache_read":%s,"cache_write":%s,"cache_write_1h":%s,"output":%s},' \
      "$(jnum "$uncached")" "$(jnum "$cread")" "$(jnum "$cwrite")" "$(jnum "$cwrite1h")" "$(jnum "$out")"
    printf '"quality":{"success_exit":%s,"assertion_pass":%s,"files_changed":%s},' \
      "$(jnum "$succ_rc")" "$assert_pass" "$(jnum "$files_changed")"
    printf '"activation":{"compression_count":%s,"fired":%s},' "$(jnum "$cc")" "$fired"
    printf '"sdk_result_cost_usd":%s,"sdk_num_turns":%s,"sdk_is_error":%s,"model":"%s"' \
      "$(jnum "$sdk_cost")" "$(jnum "$sdk_turns")" "$(jnum "$is_err")" "$model"
    printf '}\n'
  } >> "$LEDGER"

  log "    [$run_id] status=$status est=\$${cost} turns=$turns cc=$cc succ=$succ_rc assert=$assert_pass sid=${sid:0:8}"

  # accrue spend (est list price is the budget unit) + hard cap check.
  if [[ "$cost" != "0" && "$cost" != "null" && -n "$cost" ]]; then
    CUMULATIVE="$(python3 -c "print(f'{float(\"$CUMULATIVE\")+float(\"$cost\"):.6f}')")"
  fi
  log "    cumulative est spend: \$$CUMULATIVE / \$$BUDGET_USD"
  if python3 -c "import sys; sys.exit(0 if float('$CUMULATIVE')>float('$BUDGET_USD') else 1)"; then
    warn "BUDGET CAP BREACHED: \$$CUMULATIVE > \$$BUDGET_USD — aborting remaining sessions."
    ABORTED=1
  fi
}

# --- the ONE paired block --------------------------------------------
for task in "${TASKS[@]}"; do
  [[ "$ABORTED" -eq 1 ]] && break
  read -r first second < <(pair_order "$task")
  log "block $BLOCK · task $task · order: $first then $second"
  run_session "$task" "$first" 0
  [[ "$ABORTED" -eq 1 ]] && break
  run_session "$task" "$second" 1
done

log ""
log "== pilot block complete =="
log "sessions ledgered: $(wc -l < "$LEDGER")"
log "cumulative est list-price spend: \$$CUMULATIVE (cap \$$BUDGET_USD)"
[[ "$ABORTED" -eq 1 ]] && log "NOTE: run ABORTED early (budget) — see ledger."
log "ledger:   $LEDGER"
log "manifest: $RUN_MANIFEST"
log ""
log "next: analyze.sh --pilot --ledger $LEDGER"
