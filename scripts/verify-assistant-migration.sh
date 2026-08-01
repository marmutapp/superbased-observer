#!/usr/bin/env bash
# verify-assistant-migration.sh — assert the committed relabel migration
# internal/db/migrations/078_assistant_text_action_type_relabel.sql
# matches a fresh run of internal/db/asstbackfillgen.
#
# asstbackfillgen is the SOLE writer of that file. It transposes the
# `<tool>.assistant_text` emit-site inventory — the adapters that record
# the model's per-message natural-language response text — into the
# historical repair that finishes the WP-T6/B2 relabel sweep
# (task_complete -> assistant_message). A hand-edit, or an emit site that
# moved without a matching `make assistant-migration-build`, is drift
# between the SQL and the adapters it claims to follow.
#
# This is the SIBLING gate of verify-taxonomy-migration.sh, not a
# replacement: 077 is sourced from internal/tooltax and is unknown-only,
# 078 rewrites rows an adapter already classified. Regenerating 077 must
# never absorb 078's rewrite, which is why the two generators, the two
# artifacts and the two gates stay separate.
#
# Note the migration is APPLY-ONCE by nature: regenerating it after it
# has shipped changes bytes a user's database has already consumed
# (migrations are keyed by version, not by content). Until the release
# that carries 078 is out, keep it in sync; after that, an emit-site
# change needs a NEW migration, not a regenerated 078 — which this gate
# will tell you about by failing.
#
# Like verify-taxonomy-migration.sh, this NEVER mutates the working tree
# and NEVER shells out to git: it regenerates into a scratch directory
# and byte-diffs against the committed file.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

COMMITTED_DIR="internal/db/migrations"
ARTIFACT="078_assistant_text_action_type_relabel.sql"

if [ ! -d "internal/db/asstbackfillgen" ]; then
    echo "verify-assistant-migration: missing internal/db/asstbackfillgen" >&2
    exit 1
fi
if [ ! -f "$COMMITTED_DIR/$ARTIFACT" ]; then
    echo "verify-assistant-migration: missing $COMMITTED_DIR/$ARTIFACT — run 'make assistant-migration-build' and commit" >&2
    exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

go run ./internal/db/asstbackfillgen -outdir "$tmpdir" \
    || { echo "verify-assistant-migration: asstbackfillgen failed" >&2; exit 1; }

if [ ! -f "$tmpdir/$ARTIFACT" ]; then
    echo "verify-assistant-migration: asstbackfillgen did not emit $ARTIFACT" >&2
    exit 1
fi

if ! diff -u "$COMMITTED_DIR/$ARTIFACT" "$tmpdir/$ARTIFACT" > "$tmpdir/.diff" 2>&1; then
    echo "verify-assistant-migration: $COMMITTED_DIR/$ARTIFACT drifted from a fresh asstbackfillgen run"
    echo "----- diff: committed (-) vs rebuilt (+) -----"
    cat "$tmpdir/.diff"
    echo "-----"
    echo "assistant-text migration drift detected; run 'make assistant-migration-build' and commit" >&2
    exit 1
fi

echo "assistant-text relabel migration: $COMMITTED_DIR/$ARTIFACT in sync with the emit-site table"
