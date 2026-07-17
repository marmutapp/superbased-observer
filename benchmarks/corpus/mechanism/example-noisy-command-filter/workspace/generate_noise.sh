#!/usr/bin/env bash
# generate_noise.sh — deterministic, secret-free noisy command output with
# exactly one planted failure line. Used by the mechanism-positive task to
# make the shell-output filter fire hard.
#
# Honesty / write-filter caution (§6): every line is plain "line N ..."
# prose. NO "<word>:<value>" secret-shaped tokens (the repo write path
# corrupts those). The planted marker is FAILURE_MARKER_7F3A — a bare
# token, no colon.

set -euo pipefail

TOTAL="${1:-4000}"
FAIL_AT="${2:-2731}"

for ((n = 1; n <= TOTAL; n++)); do
  if [[ "$n" -eq "$FAIL_AT" ]]; then
    echo "line $n step check FAILURE_MARKER_7F3A the widget did not converge"
  else
    echo "line $n step ok everything nominal iteration proceeding"
  fi
done
