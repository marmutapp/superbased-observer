#!/usr/bin/env bash
# build-distribution-readmes.sh — regenerate npm/observer/README.md and
# pypi/observer/README.md from their channel-specific templates by
# substituting the shared body block (docs/distribution/README-body.md).
#
# Source of truth:
#   * docs/distribution/README-body.md  — the shared middle (Per-AI-client
#                                          setup through Configuration);
#                                          identical text on both registries.
#   * npm/observer/README.template.md   — channel-specific header (title,
#                                          badges, install, quickstart step 1)
#                                          + INCLUDE marker + channel-specific
#                                          troubleshooting + footer.
#   * pypi/observer/README.template.md  — same shape for the PyPI channel.
#
# Each template carries exactly one substitution marker:
#   <!-- @@INCLUDE:docs/distribution/README-body.md@@ -->
# The build replaces that single line with the literal contents of the
# referenced file, then writes the result to the channel's README.md.
#
# Run via `make sync-distribution-readmes`; CI's
# `make verify-distribution-readmes` re-runs this script into a temp
# location and fails on drift.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BODY_PATH="docs/distribution/README-body.md"
MARKER='<!-- @@INCLUDE:docs/distribution/README-body.md@@ -->'

if [ ! -f "$BODY_PATH" ]; then
    echo "build-distribution-readmes: missing $BODY_PATH" >&2
    exit 1
fi

for channel in npm pypi; do
    template="$channel/observer/README.template.md"
    output="$channel/observer/README.md"

    if [ ! -f "$template" ]; then
        echo "build-distribution-readmes: missing $template" >&2
        exit 1
    fi

    # Exactly one marker per template; refuse to silently no-op otherwise.
    count=$(grep -cF "$MARKER" "$template" || true)
    if [ "$count" != "1" ]; then
        echo "build-distribution-readmes: $template has $count include markers, want exactly 1" >&2
        exit 1
    fi

    # Multi-line substitution via python (sed/awk multi-line + special-char
    # handling is fragile; the project already requires python3 elsewhere).
    python3 - "$template" "$output" "$MARKER" "$BODY_PATH" <<'PY'
import pathlib, sys
template, output, marker, body_path = sys.argv[1:5]
body = pathlib.Path(body_path).read_text()
content = pathlib.Path(template).read_text()
# The marker sits on its own line; strip the trailing newline on the body
# so a template line `<MARKER>\n` becomes `<body>\n` (no extra blank line).
content = content.replace(marker, body.rstrip("\n"))
pathlib.Path(output).write_text(content)
PY

    echo "build-distribution-readmes: rebuilt $output from $template + $BODY_PATH"
done
