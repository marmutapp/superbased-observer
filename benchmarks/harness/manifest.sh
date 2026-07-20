#!/usr/bin/env bash
# manifest.sh — drift / version manifest collector (§3.4).
#
# COLLECTION CODE ONLY, LOCAL-ONLY, safe to run: hashes the observer
# binary, reads `observer --version`, hashes the harness config + corpus
# manifest, and derives a pricing-table "version" from the pricing source
# file (there is no explicit pricing-table version constant yet — see the
# OPEN item below). No network, no spend, no measured results.
#
# Emits JSON to stdout. Later phases embed this into every result
# artifact; a material change in any field AUTO-EXPIRES the dependent
# website cards (§4.3).
#
# Usage: manifest.sh [--observer-bin <path>]

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"

OBS_BIN=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --observer-bin) shift; OBS_BIN="$1" ;;
    -h|--help) echo "Usage: $0 [--observer-bin <path>]"; exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
  shift
done

REPO="$(repo_root)"
[[ -n "$OBS_BIN" ]] || OBS_BIN="$REPO/bin/observer"

sha_or_null() { [[ -f "$1" ]] && printf '"%s"' "$(sha256sum "$1" | awk '{print $1}')" || printf 'null'; }
str_or_null() { [[ -n "$1" ]] && printf '"%s"' "$1" || printf 'null'; }

# observer binary + version (executing --version is a local process; no
# network). Degrade gracefully when the binary is not built.
obs_hash="null"; obs_ver="null"
if [[ -x "$OBS_BIN" ]]; then
  obs_hash="\"$(sha256sum "$OBS_BIN" | awk '{print $1}')\""
  obs_ver="\"$("$OBS_BIN" --version 2>/dev/null | head -1 | tr -d '"' || echo unknown)\""
fi

# harness provenance
git_commit="$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)"
arms_hash="$(sha_or_null "$HERE/arms.toml")"
corpus_manifest="$REPO/benchmarks/corpus/manifest.json"
corpus_hash="$(sha_or_null "$corpus_manifest")"

# pricing table: no explicit version constant exists (OPEN, §7 Q11), so
# the "version" is the content hash of the pricing source + the grepped
# "pricing as of" date marker. Honest and reproducible until a real
# version constant lands.
pricing_src="$REPO/internal/intelligence/cost/pricing.go"
pricing_hash="$(sha_or_null "$pricing_src")"
pricing_asof="$(grep -oiE 'pricing as of [0-9]{4}(-[0-9]{2})?' "$pricing_src" 2>/dev/null | head -1 | sed 's/[Pp]ricing as of //' || true)"

os_name="$(uname -s 2>/dev/null || echo unknown)"
arch="$(uname -m 2>/dev/null || echo unknown)"

cat <<JSON
{
  "schema": 1,
  "collected_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "note": "drift manifest — collection only; NO measured results here",
  "observer": {
    "binary_path": "$OBS_BIN",
    "binary_sha256": $obs_hash,
    "version": $obs_ver
  },
  "harness": {
    "git_commit": "$git_commit",
    "arms_config_sha256": $arms_hash,
    "corpus_manifest_sha256": $corpus_hash
  },
  "pricing": {
    "source_file": "internal/intelligence/cost/pricing.go",
    "source_sha256": $pricing_hash,
    "as_of": $(str_or_null "$pricing_asof"),
    "explicit_version": null,
    "open_item": "no pricing-table version constant yet (plan §7 Q11)"
  },
  "model": {
    "snapshot_id": null,
    "returned_model": null,
    "note": "filled by the result extractor from api_turns at run time (§3.4)"
  },
  "environment": {
    "os": "$os_name",
    "arch": "$arch",
    "container_image": null
  }
}
JSON
