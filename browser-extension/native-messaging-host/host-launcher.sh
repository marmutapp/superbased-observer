#!/usr/bin/env bash
# host-launcher.sh — the executable the native-messaging host manifest "path"
# points at (Chrome requires an executable, and passes no args). It execs the
# Node stdio host in this directory. Set OBSERVER_BIN / OBSERVER_CONFIG in the
# environment (or edit below) so the host can find the observer binary + the
# daemon's config.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec node "$DIR/host.js"
