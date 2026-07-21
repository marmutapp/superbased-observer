#!/usr/bin/env bash
# extract.sh — result extractor (SCAFFOLD; §3.5).
#
# Phase 0: NO live extraction. This documents the exact cache-vector the
# extractor pulls from a run's observer DB and offers a --schema mode that
# prints the read-only SQL WITHOUT connecting to any DB. Wired to real
# per-block DBs after the Phase 0a gate.
#
# The cost source of record is api_turns (proxy-accurate provider-reported
# usage) with the cache_read / cache_creation / uncached-input split,
# cross-checked against the SDK result event, priced via the versioned
# pricing table. Labelled "estimated list price", never "the bill" (§3.5).
#
# Usage:
#   extract.sh --schema                      print the read-only SQL, connect to nothing
#   extract.sh --db <path> --session <sid>   whole-task cache vector for one session
#                          [--min-id N]      scope to rows id > N (this-run guard)
#
# --db mode (Phase 0a+) opens the DB READ-ONLY (mode=ro), runs the §3.5
# whole-task cache-vector query for exactly ONE session_id (never a
# blanket scan — so a concurrent orchestrator session's turns can never
# leak into a benchmark row), and prints a single compact JSON object.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"

MODE=""
DB=""
SID=""
MINID="0"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --schema) MODE="schema" ;;
    --db) MODE="db"; shift; DB="$1" ;;
    --session) shift; SID="$1" ;;
    --min-id) shift; MINID="$1" ;;
    -h|--help) echo "Usage: $0 --schema | --db <path> --session <sid> [--min-id N]"; exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
  shift
done

if [[ "$MODE" == "db" ]]; then
  [[ -n "$DB" ]]  || die "--db requires a path"
  [[ -f "$DB" ]]  || die "db not found: $DB"
  [[ -n "$SID" ]] || die "--db mode requires --session <sid> (never a blanket scan)"
  # READ-ONLY connection (mode=ro). Whole-task total is the PRIMARY
  # endpoint (§3.1): SUM over ALL turns of this session incl. retries/
  # continuation/failed turns. error_class rows (pre-flight 401s / 5xx)
  # carry no usage and are excluded. id > min-id scopes to this run.
  read -r turns uncached cread cwrite cwrite1h out cost cc model <<<"$(
    sqlite3 "file:$DB?mode=ro" -readonly -separator ' ' "
      SELECT
        COUNT(*),
        COALESCE(SUM(input_tokens),0),
        COALESCE(SUM(cache_read_tokens),0),
        COALESCE(SUM(cache_creation_tokens),0),
        COALESCE(SUM(cache_creation_1h_tokens),0),
        COALESCE(SUM(output_tokens),0),
        ROUND(COALESCE(SUM(cost_usd),0.0),6),
        COALESCE(SUM(compression_count),0),
        COALESCE((SELECT model FROM api_turns
                   WHERE session_id='$SID' AND id > $MINID AND provider='anthropic'
                   GROUP BY model ORDER BY COUNT(*) DESC LIMIT 1),'')
      FROM api_turns
      WHERE session_id='$SID' AND id > $MINID AND provider='anthropic'
        AND (error_class IS NULL OR error_class='');
    "
  )"
  turns="${turns:-0}"
  printf '{"session_id":"%s","turns":%s,"uncached_input":%s,"cache_read":%s,"cache_write":%s,"cache_write_1h":%s,"output":%s,"est_list_price_usd":%s,"compression_count":%s,"model":"%s"}\n' \
    "$SID" "${turns:-0}" "${uncached:-0}" "${cread:-0}" "${cwrite:-0}" "${cwrite1h:-0}" "${out:-0}" "${cost:-0}" "${cc:-0}" "${model}"
  exit 0
fi
[[ "$MODE" == "schema" ]] || die "pass --schema (Phase 0) or --db (Phase 0a+)"

# The whole-task cache-vector query (§3.5). Whole-task total is the
# PRIMARY endpoint (§3.1): sum over ALL turns of a task incl. retries,
# summaries, retrieval calls, and failed turns. Printed only — not run.
cat <<'SQL'
-- READ-ONLY. Per (session,arm) whole-task cache vector + est. list-price cost.
-- Source of record: api_turns (proxy-accurate). Cross-check total against
-- the SDK result event's total_cost_usd. cachetrack events are DIAGNOSTICS
-- (tools_changed / prefix_churn), never the decisive gate (finding 8).
SELECT
  session_id,
  COUNT(*)                                    AS turns,          -- incl. retries/failed (§3.1)
  COALESCE(SUM(input_tokens), 0)              AS uncached_input,
  COALESCE(SUM(cache_read_tokens), 0)         AS cache_read,
  COALESCE(SUM(cache_creation_tokens), 0)     AS cache_write,
  COALESCE(SUM(cache_creation_1h_tokens), 0)  AS cache_write_1h,
  COALESCE(SUM(output_tokens), 0)             AS output,
  ROUND(COALESCE(SUM(cost_usd), 0.0), 6)      AS est_list_price_usd  -- pricing-table vN, NOT the bill
FROM api_turns
WHERE provider = 'anthropic'
  AND (error_class IS NULL OR error_class = '')   -- excludes pre-flight 401s; retries handled per prereg exclusion rules (§3.0)
GROUP BY session_id;
SQL

log ""
log "(--schema mode: printed SQL only. NO DB was opened, NO network, NO spend.)"
