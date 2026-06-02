# VS Code Extension

> **SuperBased Observer for VS Code** — one local intelligence layer for
> every AI coding tool you use, available without leaving your editor.
>
> _This document is the **reference** — commands, settings, surfaces._
> _Looking for the workflow walkthrough? Try the in-editor_
> _**Get Started** view (Help → Welcome), or read_
> _[`docs/vscode-extension-user-guide.md`](./vscode-extension-user-guide.md)_
> _for the long-form prose version._

The extension wraps the existing observer binary (CLI + dashboard +
proxy + MCP server) with a UX shell that surfaces the most-used
numbers + actions inside VS Code, Cursor, VSCodium, and Windsurf.
Every byte of business logic stays in Go; the extension never sends
data anywhere except to the local daemon on `127.0.0.1`.

## Install

### From the VS Code Marketplace

```bash
code --install-extension superbased.superbased-observer
```

Or search **"SuperBased Observer"** in the Extensions view
(`Ctrl+Shift+X` / `⌘⇧X`).

### From Open VSX (Cursor, VSCodium, Windsurf)

The same VSIX is published to [open-vsx.org](https://open-vsx.org/)
so Cursor and other VS Code forks pick it up via their default
registry.

```bash
cursor --install-extension superbased.superbased-observer
codium --install-extension superbased.superbased-observer
```

### From a `.vsix` (offline / private)

Per-platform `.vsix` archives ride along with every observer release;
grab the matching one from the
[Releases page](https://github.com/marmutapp/superbased-observer/releases)
and install via `code --install-extension <file>.vsix`.

## What it does

| Surface | Description |
|---|---|
| **Status bar** | Today's spend in the bottom-right (`$(graph) $313.85`), updated every 60 s. Click → open dashboard. Hover for delta vs yesterday, top model, burn rate, month projection. |
| **Activity bar** | An `observer` view container with four `TreeView`s — **Today**, **Sessions**, **Discovery**, **Costs (7 d)**. Right-click any session row for **Open in Dashboard** / **Copy Session ID**. |
| **Dashboard webview** | `Observer: Open Dashboard` opens the full React analytics SPA inside an editor tab. Same surface as the standalone `http://127.0.0.1:8081/` — Remote-SSH / Codespaces work via `portMapping`. |
| **File-freshness decorations** | A small dot in the explorer next to files your AI tools touched in the last 24 h, with a Markdown hover showing last-read-by, edit count, stale re-read count, and the tools that touched it. |
| **Budget + watcher-lag notifications** | When spend hits 80 % of `intelligence.monthly_budget_usd`, a banner suggests review. When the watcher falls behind on a session file by > 10 kB, a banner names the lagging file. Both deduped so they don't nag. |
| **Daemon lifecycle** | The extension can attach to a daemon you started in a terminal (`detect` mode — default), or spawn + supervise its own (`managed` / `auto`). Crash recovery with exponential backoff `[1 s, 2 s, 5 s]`; user-initiated stops are distinguished from crashes. |
| **Terminal profile** | A contributed terminal profile titled **"AI Coding Tool (Observer-proxied)"** that pre-exports `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, and `ENABLE_TOOL_SEARCH=true` so any AI CLI launched from it routes through the observer proxy. |
| **CodeLens on instruction files** | `CLAUDE.md`, `AGENTS.md`, and `.cursorrules` get two lenses at the top: **Refresh from Observer learnings** (runs `observer suggest --apply`) and **Preview suggestions** (dry-run, opens stdout beside the original). |

## Commands

Open the command palette (`Ctrl+Shift+P` / `⌘⇧P`) and search for
**Observer:**.

| Command | What it does |
|---|---|
| `Observer: Doctor` | Run `observer doctor` against the resolved binary in a new terminal. |
| `Observer: Start Daemon` | Spawn `observer start` (only in `managed`/`auto` mode). |
| `Observer: Stop Daemon` | SIGTERM the extension-managed daemon (graceful 5 s window before SIGKILL). |
| `Observer: Open Dashboard` | Open the dashboard webview panel. |
| `Observer: Copy Proxy Env Vars` | Copy the three proxy env vars to the clipboard in your shell's syntax (bash/zsh/fish → `export …`; PowerShell → `$env:…`; cmd → `set …`). |
| `Observer: Refresh All Trees` | Force a refresh on all four sidebar trees. |
| `Observer: Open Session in Dashboard` | Copy a session ID to clipboard + open the dashboard. (Right-click a session row.) |
| `Observer: Copy Session ID` / `Copy Path` | Self-explanatory; right-click context menu. |
| `Observer: Refresh Instructions from Learnings` | CodeLens on `CLAUDE.md` etc. — runs `observer suggest --apply --target <…>` for the current workspace. |
| `Observer: Preview Instruction Suggestions` | Same as above but dry-run; opens stdout in a new editor beside the original. |

## Settings

| Setting | Default | Purpose |
|---|---|---|
| `observer.daemon.mode` | `detect` | `detect` attaches only; `managed` spawns + kills with the editor; `auto` attaches if a daemon is running, otherwise spawns. |
| `observer.binary.path` | empty | Absolute path to override binary auto-detection. |
| `observer.dashboard.port` | `8081` | Where the dashboard listens. |
| `observer.proxy.port` | `8820` | Where the API proxy listens. |
| `observer.statusBar.enabled` | `true` | Today-spend status bar item. |

## Binary management

On first activation the extension resolves a working `observer`
binary via four-step precedence:

1. `observer.binary.path` setting (if set and the file exists).
2. `observer` on your `$PATH` (cross-platform `which`, honours
   `PATHEXT` on Windows).
3. A binary bundled inside the VSIX (per-platform packages bundle
   the matching architecture's binary at CI time).
4. Download from the latest matching observer release on GitHub,
   SHA256-verified against the same release's `SHA256SUMS`, cached
   under VS Code's `globalStorageUri/v<version>/`.

The extension version matches the observer release it was built
against, so the download URL always resolves.

## Local-first guarantees

- **No telemetry.** The extension respects `telemetry.telemetryLevel`
  and never reports usage, crash, or session data.
- **No outbound network calls** except to GitHub Releases on first
  install (for the binary, when none is found locally).
- **No data leaves your machine.** All `/api/*` traffic is to
  `127.0.0.1:<dashboard.port>` on the workspace host. Under
  Remote-SSH / Codespaces the iframe `portMapping` routes the
  webview-side `127.0.0.1` to the same loopback on the host — no
  data crosses the wire except through your existing VS Code
  remote-dev session.

## Compatibility

| Editor | Status |
|---|---|
| Visual Studio Code (≥ 1.90) | First-class |
| Cursor | Supported via Open VSX; the extension uses only stable VS Code APIs (no `proposedApi.*`) |
| VSCodium | Same as Cursor |
| Windsurf | Same as Cursor |
| Codespaces | Works — the extension declares `extensionKind: ["workspace"]` so it runs on the workspace host |
| Remote-SSH / WSL2 / Dev Containers | Same as Codespaces |

## Source

The extension lives at `vscode/` in the
[main repository](https://github.com/marmutapp/superbased-observer).
Build locally with:

```bash
cd vscode
npm install
npm run build         # bundles to out/extension.js
npm test              # runs the unit suite (118 tests at 1.7.26)
npm run package       # produces a local .vsix
```

Press **F5** inside `vscode/` to open the Extension Development
Host with the extension loaded against a dev observer.

## License

Apache-2.0 — same as the observer binary it wraps.
