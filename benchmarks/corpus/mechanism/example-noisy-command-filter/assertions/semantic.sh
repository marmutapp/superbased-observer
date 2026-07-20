#!/usr/bin/env bash
# semantic.sh — quality-guard assertion for example-noisy-command-filter.
#
# Phase 0: NOT executed by the harness. Scores whether the agent found the
# REAL planted failure (line 2731, marker FAILURE_MARKER_7F3A) rather than
# hallucinating a failure — the mechanism-stratum guard that a big byte
# saving did not come at the cost of dropping the one line that mattered.
#
# Contract: exit 0 = holds; prints `assertion=<id> pass=<0|1>` last.
# Usage (later phases): assertions/semantic.sh <workspace-dir>

set -euo pipefail

WS="${1:-.}"
id="correct-failing-line"
pass=1

answer="$WS/ANSWER.txt"
if [[ ! -f "$answer" ]]; then
  echo "FAIL: ANSWER.txt missing" >&2
  pass=0
fi

if [[ $pass -eq 1 ]] && ! grep -q 'FAILURE_MARKER_7F3A' "$answer"; then
  echo "FAIL: planted failure marker not identified" >&2
  pass=0
fi
# The correct line number is 2731 (FAIL_AT default in generate_noise.sh).
if [[ $pass -eq 1 ]] && ! grep -Eq '(^|[^0-9])2731([^0-9]|$)' "$answer"; then
  echo "FAIL: correct line number (2731) not reported" >&2
  pass=0
fi

echo "assertion=$id pass=$pass"
[[ $pass -eq 1 ]]
