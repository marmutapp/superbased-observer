#!/usr/bin/env bash
# verify-reasoning-migration.sh — assert the committed convergence migration
# internal/db/migrations/079_reasoning_row_convergence.sql
# matches a fresh run of internal/db/reasoninggen.
#
# reasoninggen is the SOLE writer of that file. It transposes three things
# into SQL: the PLACEHOLDER shapes the retired codex reasoning emit site
# could write (`(reasoning)` and `(encrypted reasoning, N bytes)`), the
# dependency protocol verified in
# internal/retention/retention.go::deleteActionsOlder, and the cursor
# `cursor.assistant_response` pair rewrite. A hand-edit, or a producer
# shape / dependency table that moved without a matching
# `make reasoning-migration-build`, is drift between the SQL and the code
# it claims to follow.
#
# This is the SIBLING gate of verify-assistant-migration.sh, not a
# replacement, and the separation is load-bearing: 078 REWRITES rows
# (task_complete -> assistant_message) while 079 DELETES them. A delete
# is a strictly more dangerous mode, so it keeps its own generator, its
# own artifact and its own gate — one regeneration mistake must never be
# able to turn a relabel into a deletion.
#
# Note the migration is APPLY-ONCE by nature, and more sharply so than
# 078: regenerating it after it has shipped changes bytes a user's
# database has already consumed (migrations are keyed by version, not by
# content), and those bytes DELETED rows. Until the release that carries
# 079 is out, keep it in sync; after that, a shape change needs a NEW
# migration, not a regenerated 079 — which this gate will tell you about
# by failing.
#
# Like its siblings, this NEVER mutates the working tree and NEVER shells
# out to git: it regenerates into a scratch directory and byte-diffs
# against the committed file.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

COMMITTED_DIR="internal/db/migrations"
ARTIFACT="079_reasoning_row_convergence.sql"

if [ ! -d "internal/db/reasoninggen" ]; then
    echo "verify-reasoning-migration: missing internal/db/reasoninggen" >&2
    exit 1
fi
if [ ! -f "$COMMITTED_DIR/$ARTIFACT" ]; then
    echo "verify-reasoning-migration: missing $COMMITTED_DIR/$ARTIFACT — run 'make reasoning-migration-build' and commit" >&2
    exit 1
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

go run ./internal/db/reasoninggen -outdir "$tmpdir" \
    || { echo "verify-reasoning-migration: reasoninggen failed" >&2; exit 1; }

if [ ! -f "$tmpdir/$ARTIFACT" ]; then
    echo "verify-reasoning-migration: reasoninggen did not emit $ARTIFACT" >&2
    exit 1
fi

if ! diff -u "$COMMITTED_DIR/$ARTIFACT" "$tmpdir/$ARTIFACT" > "$tmpdir/.diff" 2>&1; then
    echo "verify-reasoning-migration: $COMMITTED_DIR/$ARTIFACT drifted from a fresh reasoninggen run"
    echo "----- diff: committed (-) vs rebuilt (+) -----"
    cat "$tmpdir/.diff"
    echo "-----"
    echo "reasoning convergence migration drift detected; run 'make reasoning-migration-build' and commit" >&2
    exit 1
fi

echo "reasoning convergence migration: $COMMITTED_DIR/$ARTIFACT in sync with the producer-shape table"
