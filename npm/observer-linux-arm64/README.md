# @superbased/observer-linux-arm64

Pre-built **Linux arm64** binary for [`@superbased/observer`](https://www.npmjs.com/package/@superbased/observer).

You almost certainly don't want to install this directly. Install
the main package instead — npm will automatically pick up the
binary that matches your machine:

```bash
npm install -g @superbased/observer
observer --version
```

The main package declares this and four other per-platform binaries
as `optionalDependencies` with matching `os` + `cpu` constraints, so
only the right one downloads. Same shape as `esbuild` / `swc` /
`@biomejs/biome`.

## When to install this directly

- You're cross-publishing observer's binary into another tool's
  release pipeline and only need this platform's bytes.
- You're debugging a release issue where the main shim can't resolve
  the platform package.

In every other case use `@superbased/observer`.

## What you get

A single executable at `bin/observer`. No runtime dependencies,
pure-Go (CGO disabled — uses `modernc.org/sqlite`). Static assets
(dashboard HTML/CSS/JS) and SQL migrations are embedded into the
binary at build time via `go:embed`.

## See also

- [Main package](https://www.npmjs.com/package/@superbased/observer)
  — install + quickstart + dashboard tour + MCP tools + compression
  mechanisms + cost math + glossary + CLI reference + configuration +
  troubleshooting + privacy + license.
- [Source repository](https://github.com/marmutapp/superbased-observer)
  — Go source, contributor docs, full spec.

## License

Apache 2.0. See
[LICENSE](https://github.com/marmutapp/superbased-observer/blob/main/LICENSE).
