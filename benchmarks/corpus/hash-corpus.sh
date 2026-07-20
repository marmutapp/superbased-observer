#!/usr/bin/env bash
# hash-corpus.sh — generate benchmarks/corpus/manifest.json: a
# machine-readable index of every frozen task with a content hash.
#
# LOCAL-ONLY, safe to run: reads files, computes sha256, writes
# manifest.json. No network, no observer invocation, no spend. This is
# the corpus side of the drift/claim-manifest chain (§3.4/§4.5).
#
# A task's hash covers task.toml + workspace/ + assertions/ (sorted, so
# it is stable across filesystems). A directory with no task.toml is
# recorded as a placeholder (e.g. mechanism/lumen before it is frozen).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$HERE/manifest.json"

sha_of_file() { sha256sum "$1" | awk '{print $1}'; }

# Content hash of a task dir: sha256 over the sorted list of
# "<relpath>:<filehash>" lines, so file order / mtimes don't leak in.
task_hash() {
  local dir="$1"
  local acc=""
  while IFS= read -r f; do
    local rel="${f#"$dir"/}"
    acc+="$rel:$(sha_of_file "$f")"$'\n'
  done < <(find "$dir" -type f ! -name 'manifest.json' | LC_ALL=C sort)
  printf '%s' "$acc" | sha256sum | awk '{print $1}'
}

json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

emit_task() {
  local stratum="$1" dir="$2"
  local id; id="$(basename "$dir")"
  local has_task="false" hash="null"
  if [[ -f "$dir/task.toml" ]]; then
    has_task="true"
    hash="\"$(task_hash "$dir")\""
  fi
  cat <<JSON
    {
      "id": "$(json_escape "$id")",
      "stratum": "$stratum",
      "path": "corpus/$stratum/$id",
      "frozen": $has_task,
      "hash": $hash
    }
JSON
}

{
  echo "{"
  echo "  \"schema\": 1,"
  echo "  \"generated_by\": \"benchmarks/corpus/hash-corpus.sh\","
  echo "  \"note\": \"content hashes only; NO measured results live in this file\","
  echo "  \"tasks\": ["
  first=1
  for stratum in representative mechanism; do
    [[ -d "$HERE/$stratum" ]] || continue
    for dir in "$HERE/$stratum"/*/; do
      [[ -d "$dir" ]] || continue
      dir="${dir%/}"
      if [[ $first -eq 0 ]]; then echo "    ,"; fi
      first=0
      emit_task "$stratum" "$dir"
    done
  done
  echo "  ]"
  echo "}"
} > "$OUT"

# Top-level corpus hash = hash of the emitted manifest body (post-write,
# excluding itself is unnecessary since it does not contain its own hash).
corpus_hash="$(sha_of_file "$OUT")"
echo "wrote $OUT (corpus_hash=$corpus_hash)" >&2
