# npm packages for SuperBased Observer

This directory contains the npm distribution wrappers for the
`observer` Go binary. The runtime code stays in Go (`./cmd/observer`,
`./internal/...`); these packages exist only to ship pre-built
binaries via npm so JavaScript/TypeScript ecosystems can install the
tool with `npm install -g @superbased/observer`.

## Package layout

| Directory                 | Purpose                                                            |
| ------------------------- | ------------------------------------------------------------------ |
| `observer/`               | Main package — Node shim + `optionalDependencies` per platform.    |
| `observer-linux-x64/`     | Linux x64 binary.                                                  |
| `observer-linux-arm64/`   | Linux arm64 binary.                                                |
| `observer-darwin-x64/`    | macOS Intel binary.                                                |
| `observer-darwin-arm64/`  | macOS Apple Silicon binary.                                        |
| `observer-win32-x64/`     | Windows x64 binary.                                                |

Each per-platform package's `package.json` carries `os` + `cpu`
fields so npm only installs the matching one. The main package's
`bin/observer.js` shim resolves `@superbased/observer-<platform>-<arch>`
at runtime via `require.resolve()` and spawns the binary inside.

## Versioning

All 6 packages share the same version. The version field in every
`package.json` is `"0.0.0"` in the source tree — the
`scripts/sync-npm-version.sh` script stamps the real version (from
the git tag) into all of them right before publish.

## Publishing (CI, the normal flow)

`.github/workflows/npm-release.yml` triggers on a `v*` git tag push:

1. Cross-compile 5 binaries via `GOOS=… GOARCH=… go build`.
2. Drop each binary into the matching `npm/observer-<plat>-<arch>/bin/`
   directory.
3. Run `scripts/sync-npm-version.sh ${TAG}` to stamp the version.
4. Run `npm publish --access public` for each platform package.
5. Run `npm publish --access public` for the main package last.

The workflow uses the `NPM_TOKEN_OBSERVER` repo secret (a granular
token scoped to `@superbased/observer*`).

## Publishing manually (rarely needed — only for emergencies)

```bash
# 0. From a clean git working tree at the tag you want to release.
TAG=v1.2.3
./scripts/sync-npm-version.sh "$TAG"

# 1. Cross-compile each platform. Go does this without CGO since
#    we use modernc.org/sqlite.
GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o npm/observer-linux-x64/bin/observer    ./cmd/observer
GOOS=linux   GOARCH=arm64 go build -ldflags="-s -w" -o npm/observer-linux-arm64/bin/observer  ./cmd/observer
GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o npm/observer-darwin-x64/bin/observer   ./cmd/observer
GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o npm/observer-darwin-arm64/bin/observer ./cmd/observer
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o npm/observer-win32-x64/bin/observer.exe ./cmd/observer

# 2. Make the unix binaries executable (Windows .exe doesn't need this).
chmod +x npm/observer-{linux-x64,linux-arm64,darwin-x64,darwin-arm64}/bin/observer

# 3. Publish each platform package, then the main package last.
for p in observer-linux-x64 observer-linux-arm64 observer-darwin-x64 \
         observer-darwin-arm64 observer-win32-x64 observer; do
  (cd "npm/$p" && npm publish --access public)
done
```

## Local smoke test (before publishing)

```bash
# Build just your host's binary into the matching slot:
make build-npm-local

# Then tarball + install in a temp dir:
cd npm/observer && npm pack
mkdir -p /tmp/observer-npm-test && cd /tmp/observer-npm-test
npm init -y && npm install /home/marmutapp/superbased-observer/npm/observer/superbased-observer-*.tgz
./node_modules/.bin/observer --version
```

## Why this shape

`optionalDependencies` per platform is the modern best practice for
npm-distributing a native binary. Same shape used by `esbuild`,
`@swc/cli`, `@biomejs/biome`, `prisma`, `bun`. Tradeoffs vs alternatives:

- **vs postinstall download script**: faster install, works offline
  once cached, no postinstall script (which some orgs disable).
- **vs single platform-detect-at-runtime package**: the package
  installed is small (one binary, ~25 MB) instead of containing all 5
  (~125 MB).
- **vs WASM**: not feasible — observer needs filesystem watching,
  HTTP server bindings, and SQLite, none of which are well-supported
  in Node WASM.
