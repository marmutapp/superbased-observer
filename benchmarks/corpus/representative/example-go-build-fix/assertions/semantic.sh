#!/usr/bin/env bash
# semantic.sh — quality-guard assertion for example-go-build-fix (§3.6).
#
# Phase 0: this script is NOT executed by the harness. It is a frozen,
# bash -n-clean assertion that a later phase runs, in the post-agent
# workspace copy, to score whether the fix preserved the task's semantic
# contract — not just whether `go build` passed.
#
# Contract: exit 0 = assertion holds; non-zero = quality FAIL. It prints
# a single machine-readable line `assertion=<id> pass=<0|1>` on the last
# line so the extractor can score it without parsing prose.
#
# Usage (later phases): assertions/semantic.sh <workspace-dir>

set -euo pipefail

WS="${1:-.}"
id="minimal-diff"
pass=1

main_go="$WS/main.go"
if [[ ! -f "$main_go" ]]; then
  echo "FAIL: main.go missing" >&2
  pass=0
fi

# The greet() contract must survive: same signature, same return shape.
if [[ $pass -eq 1 ]] && ! grep -Eq 'func[[:space:]]+greet\(name[[:space:]]+string\)[[:space:]]+string' "$main_go"; then
  echo "FAIL: greet(name string) string contract was altered" >&2
  pass=0
fi
if [[ $pass -eq 1 ]] && ! grep -Eq 'return[[:space:]]+"hello, "[[:space:]]*\+[[:space:]]*name' "$main_go"; then
  echo "FAIL: greet return body was rewritten" >&2
  pass=0
fi

# No stray new .go files (the prompt forbids adding files).
extra_go=$(find "$WS" -maxdepth 1 -name '*.go' ! -name 'main.go' | wc -l | tr -d ' ')
if [[ "$extra_go" != "0" ]]; then
  echo "FAIL: $extra_go unexpected new .go file(s)" >&2
  pass=0
fi

echo "assertion=$id pass=$pass"
[[ $pass -eq 1 ]]
