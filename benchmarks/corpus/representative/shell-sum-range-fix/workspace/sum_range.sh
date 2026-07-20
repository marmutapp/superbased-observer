#!/usr/bin/env bash
# sum_range.sh — print the sum of the integers 1..N (inclusive).
#
# Contract (preserved by any fix): takes N as $1, prints the inclusive
# sum on one line. The fix must keep the loop-based computation; do not
# hardcode answers and do not edit check.sh.
set -euo pipefail

N="${1:?usage: sum_range.sh N}"
total=0
# BUG: the loop stops at N-1, so N itself is never added (sum_range.sh 5
# prints 10, not 15). The minimal fix makes the range inclusive of N.
for ((i = 1; i < N; i++)); do
  total=$((total + i))
done
echo "$total"
