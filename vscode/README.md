<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="media/logo-light.png">
    <img src="media/logo-dark.png" alt="SuperBased" height="80">
  </picture>
</p>

# SuperBased Observer for VS Code

> One local intelligence layer for every AI coding tool you use.

The Observer extension lifts the local Observer daemon — its dashboard,
its proxy, its MCP server — into VS Code so you can see what your AI
tools are doing without leaving the editor.

Everything runs **locally**. No telemetry. No data leaves your machine.

## What this extension does

Observer captures, normalises, and analyses tool-call activity from
Claude Code, Codex, Cursor, Cline, Copilot, Roo Code, Continue and
several others. The extension is a thin lifecycle + UX shell around
the existing Go binary and its `/api/*` endpoints; every byte of
business logic stays in Go.

In the current release (`0.1.0`, milestone **M0**):

- Resolves the `observer` binary via a four-step precedence:
  - `observer.binary.path` setting
  - your `$PATH`
  - a binary bundled inside the VSIX (per-platform builds)
  - download from GitHub Releases, SHA256-verified
- Exposes `Observer: Doctor` — runs `observer doctor` in a terminal
  against the resolved binary.

Daemon lifecycle, status bar, dashboard webview, sidebar trees,
terminal-profile env injection, file-freshness decorations, and
Marketplace publication land in later milestones (see the
[implementation tracker](../docs/vscode-extension-tracker.md)).

## Settings

| Setting | Default | Purpose |
|---|---|---|
| `observer.daemon.mode` | `detect` | `detect` attaches only; `managed` spawns + kills with the editor; `auto` attaches if a daemon is running, otherwise spawns. |
| `observer.binary.path` | empty | Absolute path to override binary auto-detection. |
| `observer.dashboard.port` | `8081` | Where the dashboard listens. |
| `observer.proxy.port` | `8820` | Where the API proxy listens. |
| `observer.statusBar.enabled` | `true` | Today-spend status bar item (M1+). |

## Development

```bash
cd vscode
npm install
npm run build     # bundles to out/extension.js
npm test          # runs node --test on the unit suite
```

Press **F5** inside the `vscode/` folder to open the Extension
Development Host with the extension loaded.

## License

Apache-2.0. Same as the observer binary it wraps.
