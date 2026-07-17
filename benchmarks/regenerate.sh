#!/usr/bin/env bash
# regenerate.sh — top-level orchestrator (Phase 0: VALIDATE + PLAN only).
#
# The published "trust artifact" entry point (§3.13/§4.3): in later phases
# it re-runs the frozen corpus + harness end-to-end and drives card
# auto-expiry when a manifest input changes materially. In Phase 0 it does
# the safe subset only:
#   1. regenerate the corpus manifest (local hashing),
#   2. collect the drift manifest (local reads),
#   3. dry-run the harness protocol (no execution),
#   4. print the operator gates that stand between here and Phase 0a.
#
# NO benchmark run, NO network, NO spend. A real regeneration is gated on
# the Phase 0a pilot + a frozen pre-registration (see README.md).
#
# Usage: regenerate.sh [--validate]   (only --validate exists in Phase 0)

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "${1:---validate}" in
  --validate) ;;
  -h|--help) echo "Usage: $0 --validate   (Phase 0: validate + plan only)"; exit 0 ;;
  *) echo "ERROR: unknown arg: $1 — Phase 0 supports --validate only" >&2; exit 1 ;;
esac

echo "== 1. corpus manifest ==" >&2
"$HERE/corpus/hash-corpus.sh"

echo "== 2. drift manifest ==" >&2
"$HERE/harness/manifest.sh" >/dev/null && echo "drift manifest collected (stdout suppressed; run manifest.sh directly to view)" >&2

echo "== 3. harness protocol dry-run ==" >&2
"$HERE/harness/run.sh" --dry-run

echo "== 4. operator gates before Phase 0a ==" >&2
cat >&2 <<'GATES'
  [x] Freeze the representative inclusion criteria (corpus/README.md) — FROZEN 2026-07-11T20:18:01Z.
  [x] Pre-registration FROZEN (preregistration/tool-defs-trim-2026-07-11.md §12);
      MPIE 5%, operator approved via session 2026-07-12.
  [x] Operator SPEND approval for the Phase 0a pilot — hard $15 budget cap.
  [x] Protocol-freeze sign-off (§3.0/§6).
Live path: run.sh --pilot (implemented). Phase-0a pilot ran 2026-07-12;
see preregistration/pilot-report-2026-07-12.md for σ_d + computed N + go/no-go.
The FULL pre-registered run stays gated on that report's verdict (arms.toml
blocks = 0 until powered).
GATES
