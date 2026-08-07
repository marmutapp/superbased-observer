#!/usr/bin/env bash
# verify-tool-count-build.sh — assert the committed
# website/tools/tool-count-manifest.json matches a fresh run of
# tools/toolcountgen.
#
# tools/toolcountgen is the SOLE writer of that file: it partitions
# internal/integration.Tools() (the one-owner adapter capability registry)
# into editorial families via an explicit, documented fold table, and sums
# their weights into the adapterCount that website/tools/accuracy-check.mjs
# cross-checks against CLAUDE.md's own "Platform adapters (N)" prose line.
#
# Like web/taxgen and website/track-gen, it is invoked by hand (`make
# tool-count-build`), so a registry change can ship with the manifest still
# reporting last month's count — exactly the drift class this gate exists
# to close.
#
# Like verify-taxonomy-build.sh, this NEVER mutates the working tree and
# NEVER shells out to git: it regenerates into a scratch directory and
# byte-diffs against the committed file.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

COMMITTED_DIR="website/tools"
MANIFEST="tool-count-manifest.json"

if [ ! -d "tools/toolcountgen" ]; then
    echo "verify-tool-count-build: missing tools/toolcountgen" >&2
    exit 1
fi
if [ ! -f "$COMMITTED_DIR/$MANIFEST" ]; then
    echo "verify-tool-count-build: missing $COMMITTED_DIR/$MANIFEST — run 'make tool-count-build' and commit" >&2
    exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

go run ./tools/toolcountgen -outdir "$tmpdir" \
    || { echo "verify-tool-count-build: toolcountgen failed" >&2; exit 1; }

if [ ! -f "$tmpdir/$MANIFEST" ]; then
    echo "verify-tool-count-build: toolcountgen did not emit $MANIFEST" >&2
    exit 1
fi

if ! diff -u "$COMMITTED_DIR/$MANIFEST" "$tmpdir/$MANIFEST" > "$tmpdir/.diff" 2>&1; then
    echo "verify-tool-count-build: $COMMITTED_DIR/$MANIFEST drifted from a fresh toolcountgen run"
    echo "----- diff: committed (-) vs rebuilt (+) -----"
    cat "$tmpdir/.diff"
    echo "-----"
    echo "tool-count drift detected; run 'make tool-count-build' and commit" >&2
    exit 1
fi

echo "tool-count manifest: $COMMITTED_DIR/$MANIFEST in sync with internal/integration"
