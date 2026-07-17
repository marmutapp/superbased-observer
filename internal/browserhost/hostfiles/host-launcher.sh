#!/usr/bin/env bash
# host-launcher.sh — the executable the native-messaging host manifest "path"
# points at (Chrome requires an executable and passes no args). It exports the
# observer binary + config path resolved by `observer init` and execs the Node
# stdio host next to it. Regenerate with `observer init --browser`.
#
# The OBSERVER_BIN / OBSERVER_CONFIG / node values below are substituted at
# install time by internal/browserhost/hostfiles.WriteHost.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export OBSERVER_BIN="{{OBSERVER_BIN}}"
export OBSERVER_CONFIG="{{OBSERVER_CONFIG}}"
exec "{{NODE_BIN}}" "$DIR/host.js"
