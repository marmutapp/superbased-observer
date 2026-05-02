#!/usr/bin/env bash
# sync-npm-version.sh — stamp a release version into every package.json
# under npm/, including the main package's optionalDependencies refs.
#
# Usage:
#   ./scripts/sync-npm-version.sh v1.2.3
#   ./scripts/sync-npm-version.sh 1.2.3       # leading 'v' optional
#
# Why this script exists: 6 package.json files have to stay version-
# locked in lockstep, and the main package's optionalDependencies
# block has to point at the exact same version of each platform
# package or `npm install` won't pick the right one. Doing this by
# hand on every release is a recipe for one stale ref slipping
# through. The CI release workflow runs this right before `npm
# publish`.

set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <version>" >&2
  echo "       version may have a leading 'v' which will be stripped" >&2
  exit 64
fi

# Strip leading 'v' so a git tag like 'v1.2.3' becomes the npm-friendly
# '1.2.3'.
VERSION="${1#v}"

# semver-ish sanity check — keeps us from publishing a version like
# "garbage" when someone mistypes the tag. Permissive enough to allow
# pre-release suffixes (e.g. 1.2.3-rc.1) and build metadata.
if ! echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'; then
  echo "error: '$VERSION' does not look like a semver version" >&2
  exit 65
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NPM_DIR="$REPO_ROOT/npm"

if [ ! -d "$NPM_DIR" ]; then
  echo "error: $NPM_DIR not found — are you running from the repo?" >&2
  exit 66
fi

PACKAGES=(
  observer
  observer-linux-x64
  observer-linux-arm64
  observer-darwin-x64
  observer-darwin-arm64
  observer-win32-x64
)

# Use Node to update the JSON in place — preserves field ordering,
# handles escaping correctly, and is already a dep of any environment
# that publishes to npm. Falls back to nothing — no jq required.
for pkg in "${PACKAGES[@]}"; do
  pkg_path="$NPM_DIR/$pkg/package.json"
  if [ ! -f "$pkg_path" ]; then
    echo "error: missing $pkg_path" >&2
    exit 67
  fi

  node - "$pkg_path" "$VERSION" <<'EOF'
const fs = require('fs');
const [, , filePath, version] = process.argv;
const pkg = JSON.parse(fs.readFileSync(filePath, 'utf8'));
pkg.version = version;
// The main package pins each optional dep to an exact version match.
// Any other optionalDependencies ('@superbased/...') get the same
// stamp; foreign deps stay untouched.
if (pkg.optionalDependencies) {
  for (const dep of Object.keys(pkg.optionalDependencies)) {
    if (dep.startsWith('@superbased/observer-')) {
      pkg.optionalDependencies[dep] = version;
    }
  }
}
fs.writeFileSync(filePath, JSON.stringify(pkg, null, 2) + '\n');
EOF

  echo "stamped $pkg → $VERSION"
done

echo "done — $VERSION written to $(echo "${PACKAGES[*]}" | wc -w) package.json files"
