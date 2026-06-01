#!/usr/bin/env bash
# ab-claude-report.sh — pull headline numbers from both A/B observer
# DBs and print a markdown table comparing the two runs.
#
# Reads /tmp/ab-claude/{on,off}/observer.db; needs sqlite3 in PATH
# (modernc.org/sqlite is pure Go, but the user's shell sqlite3 is
# fine for a read-only SELECT).
#
# By default, only rows newer than the run-start marker written by
# ab-claude-start.sh are summarized — defends the headline from
# contamination by prior smoke tests or earlier sessions left in the
# same DB. Override with --since=<ISO-timestamp> to pick a custom
# cutoff, --all to disable the filter (legacy behavior — sums every
# row in both DBs).
#
# Output goes to stdout. Pipe into the report doc:
#   ./scripts/ab-claude-report.sh > docs/claude-code-compression-ab-results.md

set -euo pipefail

ROOT="${AB_CLAUDE_ROOT:-/tmp/ab-claude}"
ON_DB="$ROOT/on/observer.db"
OFF_DB="$ROOT/off/observer.db"
MARKER="$ROOT/.run-started-at"

since=""
for arg in "$@"; do
  case "$arg" in
    --since=*) since="${arg#--since=}" ;;
    --all)     since="__all__" ;;
    -h|--help)
      echo "Usage: $0 [--since=<ISO-timestamp>|--all]"
      echo "  Default: filter to rows newer than $MARKER (set by ab-claude-start.sh)"
      echo "  --since=<ts>: custom cutoff (e.g., 2026-05-07T04:00:00Z)"
      echo "  --all: disable cutoff (sum every row in both DBs)"
      exit 0
      ;;
    *) echo "$0: unknown arg $arg" >&2; exit 2 ;;
  esac
done

if [[ -z "$since" ]] && [[ -f "$MARKER" ]]; then
  since=$(cat "$MARKER")
fi
# WHERE-clause fragment, expanded into every aggregate query below.
since_clause=""
if [[ -n "$since" ]] && [[ "$since" != "__all__" ]]; then
  since_clause=" AND timestamp > '$since'"
fi

for db in "$ON_DB" "$OFF_DB"; do
  if [[ ! -f "$db" ]]; then
    echo "$db not found — run scripts/ab-claude-setup.sh + ab-claude-start.sh + actually drive a Claude Code session first" >&2
    exit 1
  fi
done

if ! command -v sqlite3 >/dev/null; then
  echo "sqlite3 not in PATH — install it (apt: sqlite3, brew: sqlite)" >&2
  exit 1
fi

# Per-side aggregates pulled from api_turns. Includes the four headline
# numbers + per-mechanism breakdown if compression_events is populated.
#
# error_class IS NULL filters out the pre-flight curl from
# ab-claude-start.sh (intentional 401 from a fake x-api-key — counts
# as an api_turn but isn't real session traffic). Real Claude Code
# turns succeed and have NULL error_class.
side_summary() {
  local label=$1
  local db=$2
  sqlite3 -readonly "$db" <<SQL
.headers off
.mode list
.separator "|"
SELECT
  'turns',                 COALESCE(COUNT(*), 0),
  'input_tokens',          COALESCE(SUM(input_tokens), 0),
  'output_tokens',         COALESCE(SUM(output_tokens), 0),
  'cache_read_tokens',     COALESCE(SUM(cache_read_tokens), 0),
  'cost_usd',              ROUND(COALESCE(SUM(cost_usd), 0.0), 6),
  'compression_original',  COALESCE(SUM(compression_original_bytes), 0),
  'compression_compressed',COALESCE(SUM(compression_compressed_bytes), 0),
  'compression_count',     COALESCE(SUM(compression_count), 0),
  'compression_dropped',   COALESCE(SUM(compression_dropped_count), 0)
FROM api_turns
WHERE provider = 'anthropic'
  AND (error_class IS NULL OR error_class = '')$since_clause;
SQL
}

# Read into bash arrays. side_summary returns one '|' separated row.
mapfile -t on_kv < <(side_summary on  "$ON_DB"  | tr '|' '\n')
mapfile -t off_kv < <(side_summary off "$OFF_DB" | tr '|' '\n')

# Build associative arrays from the alternating key/value lines.
declare -A ON OFF
for ((i=0; i<${#on_kv[@]}; i+=2));  do ON["${on_kv[i]}"]="${on_kv[i+1]}";   done
for ((i=0; i<${#off_kv[@]}; i+=2)); do OFF["${off_kv[i]}"]="${off_kv[i+1]}"; done

# Helpers — bytes/saved/percent.
saved_bytes_on=$((${ON[compression_original]:-0}  - ${ON[compression_compressed]:-0}))
saved_pct_on=0
if [[ ${ON[compression_original]:-0} -gt 0 ]]; then
  saved_pct_on=$(awk -v o=${ON[compression_original]} -v c=${ON[compression_compressed]} 'BEGIN{printf "%.1f", (o-c)*100.0/o}')
fi
saved_tokens_on=$(( saved_bytes_on / 4 ))

# OFF side compression numbers should be 0 (compression disabled), but
# we surface them anyway for the row consistency.
saved_bytes_off=$((${OFF[compression_original]:-0} - ${OFF[compression_compressed]:-0}))

if [[ -n "$since" ]] && [[ "$since" != "__all__" ]]; then
  cutoff_line="Cutoff: rows since $since (override with --since=<ts> or --all)"
else
  cutoff_line="Cutoff: --all (every row in both DBs is summed; pass --since for clean A/B)"
fi

cat <<MD
# Claude Code compression A/B — generated $(date -u +%Y-%m-%dT%H:%M:%SZ)

Source: $ROOT/{on,off}/observer.db
Repo:   $(cat "$ROOT/on/repo/package.json" 2>/dev/null | grep -E '"name"' | head -1 | sed 's/[",:]//g; s/^ *name *//' || echo '?')
$cutoff_line

## Headline

| Metric | Compression ON | Compression OFF | Delta |
|---|---:|---:|---:|
| API turns                 | ${ON[turns]:-0}                | ${OFF[turns]:-0}                | $(( ${ON[turns]:-0} - ${OFF[turns]:-0} )) |
| Input tokens              | ${ON[input_tokens]:-0}         | ${OFF[input_tokens]:-0}         | $(( ${ON[input_tokens]:-0} - ${OFF[input_tokens]:-0} )) |
| Output tokens             | ${ON[output_tokens]:-0}        | ${OFF[output_tokens]:-0}        | $(( ${ON[output_tokens]:-0} - ${OFF[output_tokens]:-0} )) |
| Cache-read tokens         | ${ON[cache_read_tokens]:-0}    | ${OFF[cache_read_tokens]:-0}    | $(( ${ON[cache_read_tokens]:-0} - ${OFF[cache_read_tokens]:-0} )) |
| Cost USD                  | \$${ON[cost_usd]:-0.0}             | \$${OFF[cost_usd]:-0.0}             | \$$(awk -v a=${ON[cost_usd]:-0} -v b=${OFF[cost_usd]:-0} 'BEGIN{printf "%.6f", a-b}') |
| Compression: original B   | ${ON[compression_original]:-0} | ${OFF[compression_original]:-0} | — |
| Compression: compressed B | ${ON[compression_compressed]:-0} | ${OFF[compression_compressed]:-0} | — |
| Bytes saved               | $saved_bytes_on                | $saved_bytes_off                | — |
| ~Tokens saved (B÷4)       | $saved_tokens_on               | 0                               | — |
| Bytes saved %             | ${saved_pct_on}%               | 0%                              | — |

## Per-mechanism (ON side)

MD

# Mechanism breakdown for the ON side. Skip if no compression_events
# rows (ON daemon never compressed anything in this window).
# Filter compression_events on its own timestamp column (mirrors the
# api_turns row-level filter so per-mechanism totals match the headline
# panel rather than including events from prior sessions).
ev_clause=""
if [[ -n "$since_clause" ]]; then
  ev_clause=" AND timestamp > '$since'"
fi
sqlite3 -readonly "$ON_DB" <<SQL | awk '
NR==1 { print "| Mechanism | Events | Original B | Compressed B | Saved B |"
        print "|---|---:|---:|---:|---:|" }
{ print "| " $1 " | " $2 " | " $3 " | " $4 " | " $5 " |" }
'
.headers off
.mode list
.separator "|"
SELECT mechanism, COUNT(*), SUM(original_bytes), SUM(compressed_bytes),
       SUM(original_bytes) - SUM(compressed_bytes)
FROM compression_events
WHERE 1=1$ev_clause
GROUP BY mechanism
ORDER BY (SUM(original_bytes) - SUM(compressed_bytes)) DESC;
SQL

cat <<MD

## Per-turn timing distribution

| Side | Turns | Median ms | p95 ms |
|---|---:|---:|---:|
MD

# Per-side turn timing (only available when total_response_ms is set).
for side_db in "on:$ON_DB" "off:$OFF_DB"; do
  side=${side_db%%:*}
  db=${side_db##*:}
  sqlite3 -readonly "$db" <<SQL
.headers off
.mode list
.separator "|"
WITH ranked AS (
  SELECT total_response_ms,
         ROW_NUMBER() OVER (ORDER BY total_response_ms) AS rn,
         COUNT(*) OVER () AS n
  FROM api_turns
  WHERE provider = 'anthropic' AND total_response_ms IS NOT NULL$since_clause
)
SELECT '| $side | ' || COALESCE(MAX(n), 0)
       || ' | ' || COALESCE(MAX(CASE WHEN rn = (n+1)/2 THEN total_response_ms END), 0)
       || ' | ' || COALESCE(MAX(CASE WHEN rn = (n*95+99)/100 THEN total_response_ms END), 0)
       || ' |'
FROM ranked;
SQL
done

cat <<MD

---

Generated by \`scripts/ab-claude-report.sh\`. Re-run after each Claude
Code session to refresh the numbers; the script reads both DBs
read-only.
MD
