#!/usr/bin/env bash
# verify-track-build.sh — assert website/docs-src/track/*.md, nav.toml's
# "generated:track" block, and sitemap.xml's "generated:track" block all
# match a fresh run of website/track-gen against internal/integration's
# current Capabilities().
#
# website/track-gen is the SOLE writer of the per-adapter track pages
# (docs/new-adapter-checklist.md); it derives the page roster from the
# capability registry so a new/changed adapter automatically grows or
# updates a page. But it has historically been invoked by hand
# (`go run ./website/track-gen && make docs-build`), so a registry change
# can ship without anyone re-running it. This gate closes that gap.
#
# Like verify-website-build.sh, this NEVER mutates the working tree: it
# regenerates into a scratch copy of website/docs-src + sitemap.xml and
# diffs against the committed files. track-gen writes in place (no -out
# override for the track dir specifically), so the scratch copy is what
# makes a non-mutating check possible here.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

DOCS_SRC="website/docs-src"
SITEMAP="website/sitemap-pages.xml"

if [ ! -d "$DOCS_SRC/track" ]; then
    echo "verify-track-build: missing $DOCS_SRC/track" >&2
    exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

cp -r "$DOCS_SRC" "$tmpdir/docs-src"
cp "$SITEMAP" "$tmpdir/sitemap.xml"

# -date-src points at the REAL tree. <lastmod> comes from each page's git
# history, and the scratch copy above has none — without this every page falls
# back to today's date, so the gate compares real commit dates against today
# and fails every day regardless of whether anything drifted. The scratch copy
# still does its job: nothing here writes to the working tree.
go run ./website/track-gen -docs-src "$tmpdir/docs-src" -sitemap "$tmpdir/sitemap.xml" \
    -date-src "$DOCS_SRC" \
    || { echo "verify-track-build: track-gen failed" >&2; exit 1; }

fail=0

if ! diff -rq "$DOCS_SRC/track" "$tmpdir/docs-src/track" > "$tmpdir/.diff-track"; then
    echo "verify-track-build: $DOCS_SRC/track drifted from a fresh track-gen run"
    cat "$tmpdir/.diff-track"
    fail=1
fi

if ! diff -u "$DOCS_SRC/nav.toml" "$tmpdir/docs-src/nav.toml" > "$tmpdir/.diff-nav"; then
    echo "verify-track-build: $DOCS_SRC/nav.toml's generated:track block drifted"
    echo "----- diff: committed (a) vs rebuilt (b) -----"
    cat "$tmpdir/.diff-nav"
    echo "-----"
    fail=1
fi

if ! diff -u "$SITEMAP" "$tmpdir/sitemap.xml" > "$tmpdir/.diff-sitemap"; then
    echo "verify-track-build: $SITEMAP's generated:track block drifted"
    echo "----- diff: committed (a) vs rebuilt (b) -----"
    cat "$tmpdir/.diff-sitemap"
    echo "-----"
    fail=1
fi

if [ "$fail" != "0" ]; then
    echo "track-page drift detected; run 'make track-build' and commit" >&2
    exit 1
fi

echo "track pages: in sync with internal/integration.Capabilities()"
