#!/usr/bin/env bash
# semantic.sh — quality-guard assertion for go-pricing-refactor (§3.6).
#
# Phase 0: NOT executed by the harness. Because the suite is GREEN at task
# start, `go test` cannot tell a real refactor from a no-op. This guard
# does: it verifies the duplicated rounding divisor collapsed to a single
# site, both functions survive, and the test is untouched. A "did nothing"
# arm passes the completion gate but FAILS here (§3.6, criterion #3).
#
# The pricing.go doc comment is deliberately free of the digits 100/108/50
# so the divisor count below reads code, not prose.
#
# Contract: exit 0 = holds; prints `assertion=<id> pass=<0|1>` last.
# Usage (later phases): assertions/semantic.sh <workspace-dir>

set -euo pipefail

WS="${1:-.}"
id="duplication-removed"
pass=1

src="$WS/pricing.go"
tst="$WS/pricing_test.go"

if [[ ! -f "$src" || ! -f "$tst" ]]; then
  echo "FAIL: pricing.go or pricing_test.go missing" >&2
  pass=0
fi

# Both functions must survive (no deleting one to "reduce duplication").
for sig in 'func priceWithTax(cents int) int' 'func bulkPriceWithTax(cents, qty int) int'; do
  if [[ $pass -eq 1 ]] && ! grep -qF "$sig" "$src"; then
    echo "FAIL: $sig was removed/altered" >&2
    pass=0
  fi
done

# Duplication removed: the rounding divisor 100 must now appear <= 1 time
# in pricing.go (2 pre-refactor). A no-op arm leaves 2 -> FAIL.
n_div=$(grep -o '100' "$src" | wc -l | tr -d ' ')
if [[ $pass -eq 1 && "$n_div" -gt 1 ]]; then
  echo "FAIL: rounding divisor still duplicated ($n_div sites; expected <= 1)" >&2
  pass=0
fi

# Test file unchanged: distinctive cases must remain.
for needle in 'TestPriceWithTax' 'TestBulkPriceWithTax' '{100, 3, 324}'; do
  if [[ $pass -eq 1 ]] && ! grep -qF "$needle" "$tst"; then
    echo "FAIL: pricing_test.go was modified (missing: $needle)" >&2
    pass=0
  fi
done

# No stray new .go files.
extra_go=$(find "$WS" -maxdepth 1 -name '*.go' ! -name 'pricing.go' ! -name 'pricing_test.go' | wc -l | tr -d ' ')
if [[ $pass -eq 1 && "$extra_go" != "0" ]]; then
  echo "FAIL: $extra_go unexpected new .go file(s)" >&2
  pass=0
fi

echo "assertion=$id pass=$pass"
[[ $pass -eq 1 ]]
