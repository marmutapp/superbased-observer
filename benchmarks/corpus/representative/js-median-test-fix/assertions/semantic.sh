#!/usr/bin/env bash
# semantic.sh — quality-guard assertion for js-median-test-fix (§3.6).
#
# Phase 0: NOT executed by the harness. A frozen, `bash -n`-clean
# assertion a later phase runs in the post-agent workspace copy to score
# whether the fix landed in the SOURCE and preserved the contract — not
# whether a cheaper arm simply weakened the test to go green.
#
# Contract: exit 0 = holds; non-zero = quality FAIL. Prints a single
# machine-readable last line `assertion=<id> pass=<0|1>`.
# Usage (later phases): assertions/semantic.sh <workspace-dir>

set -euo pipefail

WS="${1:-.}"
id="fix-source-not-test"
pass=1

src="$WS/median.js"
tst="$WS/median.test.js"

if [[ ! -f "$src" || ! -f "$tst" ]]; then
  echo "FAIL: median.js or median.test.js missing" >&2
  pass=0
fi

# The median() export contract must survive.
if [[ $pass -eq 1 ]] && ! grep -Eq 'function[[:space:]]+median[[:space:]]*\(' "$src"; then
  echo "FAIL: median() function contract was removed/renamed" >&2
  pass=0
fi
if [[ $pass -eq 1 ]] && ! grep -Eq 'module\.exports[[:space:]]*=[[:space:]]*\{[[:space:]]*median' "$src"; then
  echo "FAIL: median export was removed" >&2
  pass=0
fi

# The test suite must not be weakened: the even-case 2.5 expectation and
# all four test() cases must remain.
if [[ $pass -eq 1 ]] && ! grep -q '2.5' "$tst"; then
  echo "FAIL: the even-length 2.5 expectation was removed from the test" >&2
  pass=0
fi
n_tests=$(grep -Eco '\btest\(' "$tst" || true)
if [[ $pass -eq 1 && "$n_tests" != "4" ]]; then
  echo "FAIL: expected 4 test() cases, found $n_tests (test weakened/deleted)" >&2
  pass=0
fi

# No stray new .js files (the prompt forbids adding files).
extra_js=$(find "$WS" -maxdepth 1 -name '*.js' ! -name 'median.js' ! -name 'median.test.js' | wc -l | tr -d ' ')
if [[ $pass -eq 1 && "$extra_js" != "0" ]]; then
  echo "FAIL: $extra_js unexpected new .js file(s)" >&2
  pass=0
fi

echo "assertion=$id pass=$pass"
[[ $pass -eq 1 ]]
