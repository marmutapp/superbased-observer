#!/usr/bin/env bash
# common.sh — shared helpers for the benchmark harness. Sourced, not run.
#
# Deliberately dependency-light: a MINIMAL flat-TOML reader (grep/awk),
# logging, and a NO-NETWORK / NO-SPEND guard that later phases call before
# any live step. Phase 0 never executes a live step; these helpers exist
# so the dry-run validator and the manifest collector share one
# implementation.

# ---- paths -----------------------------------------------------------
# BENCH_ROOT = benchmarks/ ; REPO_ROOT = repo root (best-effort).
bench_root() { cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd; }
repo_root() {
  local r
  r="$(git -C "$(bench_root)" rev-parse --show-toplevel 2>/dev/null)" || r="$(bench_root)/.."
  ( cd "$r" && pwd )
}

# ---- logging ---------------------------------------------------------
log()  { printf '%s\n' "$*" >&2; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# ---- minimal flat-TOML reader ----------------------------------------
# toml_get <file> <key> — first top-level `key = value`, quotes/comment
# stripped. Good enough for the flat scalar keys this harness uses; NOT a
# general TOML parser (documented limitation).
toml_get() {
  local file="$1" key="$2"
  awk -v k="$key" '
    /^[[:space:]]*#/ { next }
    {
      line=$0
      sub(/#.*/, "", line)
      if (match(line, "^[[:space:]]*" k "[[:space:]]*=")) {
        v=line
        sub("^[[:space:]]*" k "[[:space:]]*=[[:space:]]*", "", v)
        gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
        gsub(/^"|"$/, "", v)
        print v
        exit
      }
    }' "$file"
}

# toml_array <file> <key> — a single-line `key = ["a", "b"]` → newline
# list. Minimal: one-line arrays of quoted strings only.
toml_array() {
  local file="$1" key="$2"
  local raw; raw="$(toml_get "$file" "$key")"
  [[ -z "$raw" ]] && return 0
  printf '%s' "$raw" | tr -d '[]"' | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -v '^$' || true
}

# list_arms <file> — one line per [[arm]]: id|tool|compression.
list_arms() {
  local file="$1"
  awk '
    /^[[:space:]]*\[\[arm\]\]/ { if (inarm) emit(); inarm=1; id=""; tool=""; comp=""; next }
    /^[[:space:]]*\[/          { if (inarm) { emit(); inarm=0 } }
    inarm && /^[[:space:]]*id[[:space:]]*=/          { id=val() }
    inarm && /^[[:space:]]*tool[[:space:]]*=/        { tool=val() }
    inarm && /^[[:space:]]*compression[[:space:]]*=/ { comp=val() }
    END { if (inarm) emit() }
    function val(   v) { v=$0; sub(/#.*/,"",v); sub(/^[^=]*=[[:space:]]*/,"",v); gsub(/^[[:space:]]*"|"[[:space:]]*$/,"",v); gsub(/^[[:space:]]+|[[:space:]]+$/,"",v); return v }
    function emit() { if (id!="") print id "|" tool "|" comp }
  ' "$file"
}

# ---- no-network / no-spend guard -------------------------------------
# assert_dry_run <mode> — Phase 0 hard stop. Any non-dry-run mode must
# clear the Phase 0a gate (blocks > 0 + a frozen prereg + explicit
# approval). Until then, refuse loudly so nothing can execute or spend.
assert_phase0a_gate() {
  local arms_file="$1"
  local blocks prereg
  blocks="$(toml_get "$arms_file" blocks)"
  prereg="$(toml_get "$arms_file" prereg)"
  [[ "${blocks:-0}" -gt 0 ]] 2>/dev/null || die "blocks = ${blocks:-0}: not powered. Run the Phase 0a pilot to compute the block count (§3.1) before any live run."
  [[ -n "$prereg" && -f "$(bench_root)/$prereg" ]] || die "pre-registration '$prereg' not frozen (§3.0). Freeze it before a live run."
  die "Phase 0a gate: live execution is intentionally NOT implemented in this Phase 0 scaffold. See benchmarks/README.md 'Operator gates'."
}
