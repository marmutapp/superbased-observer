#!/usr/bin/env bash
# verify-plugins-build.sh — assert the committed plugins/ tree matches a
# fresh run of plugins/plugingen.
#
# plugins/plugingen is the SOLE writer of the in-tool plugin manifests
# (docs/plans/adapter-plugins-distribution-plan-2026-07-31.md §3). It does
# not re-describe observer's wiring: it RUNS the real `observer init`
# registrars (internal/mcp's Registrar, internal/hook's Registry) against a
# throwaway sandbox HOME and transposes what they wrote into each surface's
# manifest format. That is what keeps plugin wiring from forking silently
# from init wiring.
#
# But like website/track-gen, it is invoked by hand (`make plugins-build`),
# so a registrar change can ship without anyone re-running it. This gate
# closes that gap.
#
# Like verify-track-build.sh and verify-website-build.sh, this NEVER mutates
# the working tree: it copies plugins/ into a scratch dir, regenerates
# THERE, and diffs against the committed files.
#
# Scope note: a build-into-temp diff catches drifted and newly-added
# generated files, but not a generated file that was renamed or dropped and
# left behind on disk. That direction is asserted by
# plugins/plugingen's TestCommittedTreeHasNoStrayFiles.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

PLUGINS_DIR="plugins"

if [ ! -d "$PLUGINS_DIR/plugingen" ]; then
    echo "verify-plugins-build: missing $PLUGINS_DIR/plugingen" >&2
    exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

cp -r "$PLUGINS_DIR" "$tmpdir/plugins"

go run ./plugins/plugingen -out "$tmpdir/plugins" \
    || { echo "verify-plugins-build: plugingen failed" >&2; exit 1; }

if ! diff -r "$PLUGINS_DIR" "$tmpdir/plugins" > "$tmpdir/.diff-plugins" 2>&1; then
    echo "verify-plugins-build: $PLUGINS_DIR/ drifted from a fresh plugingen run"
    echo "----- diff: committed (a) vs rebuilt (b) -----"
    cat "$tmpdir/.diff-plugins"
    echo "-----"
    echo "plugin-manifest drift detected; run 'make plugins-build' and commit" >&2
    exit 1
fi

echo "plugin manifests: in sync with the observer init registrars"

# ----------------------------------------------------------------------
# The hand-written half: plugins/opencode/src/*.ts.
#
# The diff above proves every GENERATED file matches the registrars. It
# says nothing about the OpenCode package's hand-written SDK glue, which
# the Go tests can only inspect as substrings — so a TypeScript syntax
# error that happens to preserve those substrings would ship and surface
# at the operator's `npm publish`, long after CI was green. This step
# parses the real source.
#
# HONEST SCOPE — this is a PARSE/BUNDLE gate, not a TYPE check. Type
# checking index.ts needs @opencode-ai/plugin's declarations, which are
# not vendored anywhere in this repo, and installing them here would mean
# an npm install of a third-party SDK inside CI for a package we do not
# even publish from CI. So: syntax, imports and module shape are gated;
# a type error against the OpenCode SDK is NOT, and is caught by
# `npm run build` in plugins/opencode at publish time (README documents
# that step). esbuild erases the type-only import, which is exactly why
# it can parse the file without the SDK present.
#
# Reuses the esbuild already vendored in web/node_modules — the same
# lever scripts/verify-taxonomy-ts.sh pulls. No new dependency, and like
# every other verify-* gate this NEVER mutates the working tree: it
# bundles into the same scratch dir.
# ----------------------------------------------------------------------
OPENCODE_ENTRY="$PLUGINS_DIR/opencode/src/index.ts"
ESBUILD="web/node_modules/.bin/esbuild"

if [ ! -f "$OPENCODE_ENTRY" ]; then
    echo "verify-plugins-build: missing $OPENCODE_ENTRY" >&2
    exit 1
fi
if [ ! -x "$ESBUILD" ]; then
    echo "verify-plugins-build: missing $ESBUILD — run 'cd web && npm ci'" >&2
    exit 1
fi

# --bundle would try to resolve "@opencode-ai/plugin"; the import is
# type-only and erased, but esbuild still walks the import graph in bundle
# mode, so this transpiles WITHOUT bundling and lets the relative import
# of the generated module be checked by compiling the whole src/ dir.
"$ESBUILD" "$PLUGINS_DIR"/opencode/src/*.ts \
    --format=esm \
    --platform=node \
    --log-level=warning \
    --outdir="$tmpdir/opencode-ts" \
    || { echo "verify-plugins-build: esbuild failed to parse the OpenCode plugin source" >&2; exit 1; }

# The transpiled entry must still import the generated wiring module and
# must not have had its default export erased — a parse gate that accepted
# an empty file would prove nothing.
node --input-type=module -e '
import { readFileSync } from "node:fs";
const out = readFileSync(process.argv[1], "utf8");
for (const needle of ["wiring.generated", "OBSERVER_MCP_SERVER"]) {
  if (!out.includes(needle)) {
    console.error(`verify-plugins-build: transpiled OpenCode plugin lost ${needle}`);
    process.exit(1);
  }
}
if (!/export\s*{/.test(out) && !/export default/.test(out)) {
  console.error("verify-plugins-build: transpiled OpenCode plugin exports nothing");
  process.exit(1);
}
' "$tmpdir/opencode-ts/index.js"

echo "opencode plugin source: parses, and still exports the generated wiring"
