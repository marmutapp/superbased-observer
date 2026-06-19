// Embedded dashboard webview.
//
// Hosts the existing React SPA (web/src) at http://127.0.0.1:<port>/
// inside a WebviewPanel. portMapping rewrites the iframe's loopback
// to the extension-host loopback so Remote-SSH / Codespaces work
// without operator intervention. Idle state surfaces a "Start Daemon"
// button that postMessages the host so the user can recover without
// leaving the panel.

import * as vscode from 'vscode';
import type { DaemonManager, DaemonState } from '../daemon';
import { generateNonce } from './nonce';
import { output } from '../output';

const VIEW_TYPE = 'observerDashboard';
const TITLE = 'Observer Dashboard';

export class DashboardPanel implements vscode.Disposable {
  private static current: DashboardPanel | undefined;

  private readonly panel: vscode.WebviewPanel;
  private readonly daemon: DaemonManager;
  private readonly disposables: vscode.Disposable[] = [];
  private disposed = false;

  static createOrShow(daemon: DaemonManager): DashboardPanel {
    const column = vscode.window.activeTextEditor?.viewColumn ?? vscode.ViewColumn.Active;
    if (DashboardPanel.current) {
      DashboardPanel.current.panel.reveal(column);
      DashboardPanel.current.render();
      return DashboardPanel.current;
    }
    const port = daemon.getState().dashboardPort;
    const panel = vscode.window.createWebviewPanel(VIEW_TYPE, TITLE, column, {
      enableScripts: true,
      retainContextWhenHidden: true,
      portMapping: [{ webviewPort: port, extensionHostPort: port }],
      localResourceRoots: [],
    });
    DashboardPanel.current = new DashboardPanel(panel, daemon);
    return DashboardPanel.current;
  }

  private constructor(panel: vscode.WebviewPanel, daemon: DaemonManager) {
    this.panel = panel;
    this.daemon = daemon;

    this.panel.onDidDispose(() => this.dispose(), null, this.disposables);

    this.panel.webview.onDidReceiveMessage(
      (msg) => this.handleMessage(msg),
      null,
      this.disposables,
    );

    this.disposables.push(this.daemon.onDidChangeState(() => this.render()));

    this.render();
  }

  private render(): void {
    if (this.disposed) return;
    const state = this.daemon.getState();
    const nonce = generateNonce();
    this.panel.webview.html =
      state.status === 'idle'
        ? renderIdleHtml(nonce, state)
        : renderLiveHtml(nonce, state);
  }

  private async handleMessage(msg: unknown): Promise<void> {
    if (!msg || typeof msg !== 'object') return;
    const cmd = (msg as { cmd?: unknown }).cmd;
    switch (cmd) {
      case 'startDaemon':
        output.appendLine('Dashboard webview requested daemon start');
        await this.daemon.start();
        this.render();
        return;
      case 'reload':
        this.render();
        return;
      default:
        output.appendLine(`Dashboard webview: unknown message cmd=${String(cmd)}`);
        return;
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    DashboardPanel.current = undefined;
    while (this.disposables.length) {
      const d = this.disposables.pop();
      try {
        d?.dispose();
      } catch {
        /* best effort */
      }
    }
    this.panel.dispose();
  }
}

function buildCsp(nonce: string): string {
  return [
    "default-src 'none'",
    "frame-src http://127.0.0.1:* https:",
    `script-src 'nonce-${nonce}'`,
    "style-src 'unsafe-inline'",
    "img-src data: https:",
  ].join('; ');
}

function renderLiveHtml(nonce: string, state: DaemonState): string {
  const csp = buildCsp(nonce);
  const iframeSrc = `http://127.0.0.1:${state.dashboardPort}/`;
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta http-equiv="Content-Security-Policy" content="${csp}" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>${TITLE}</title>
    <style>
      html, body { margin: 0; padding: 0; height: 100%; background: var(--vscode-editor-background); color: var(--vscode-editor-foreground); font-family: var(--vscode-font-family); }
      iframe { width: 100%; height: 100%; border: 0; }
    </style>
  </head>
  <body>
    <iframe
      src="${iframeSrc}"
      sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
      title="${TITLE}"
    ></iframe>
    <script nonce="${nonce}">
      // No-op: the script tag exists so future panel-side helpers
      // (toolbars, reload button, etc.) can be added without
      // reworking the CSP plumbing.
    </script>
  </body>
</html>`;
}

function renderIdleHtml(nonce: string, state: DaemonState): string {
  const csp = buildCsp(nonce);
  const hint =
    state.mode === 'detect'
      ? 'Observer is not running. Run <code>observer start</code> in a terminal, or change <code>observer.daemon.mode</code> to <code>managed</code> / <code>auto</code> to let the extension start it for you.'
      : 'Observer is not running. Click below to start the extension-managed daemon.';
  const cta =
    state.mode === 'detect'
      ? '<a class="cta" href="command:workbench.action.openSettings?%22observer.daemon.mode%22">Open Observer settings</a>'
      : '<button class="cta" id="start-btn" type="button">Start Daemon</button>';
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta http-equiv="Content-Security-Policy" content="${csp}" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>${TITLE}</title>
    <style>
      html, body { margin: 0; padding: 0; height: 100%; background: var(--vscode-editor-background); color: var(--vscode-editor-foreground); font-family: var(--vscode-font-family); }
      .wrap { display: flex; align-items: center; justify-content: center; height: 100%; padding: 2rem; box-sizing: border-box; }
      .card { max-width: 32rem; padding: 2rem; border: 1px solid var(--vscode-panel-border); border-radius: 6px; background: var(--vscode-editorWidget-background); }
      h1 { margin: 0 0 0.5rem; font-size: 1.25rem; }
      p { margin: 0 0 1rem; line-height: 1.45; color: var(--vscode-descriptionForeground); }
      code { background: var(--vscode-textCodeBlock-background); padding: 0 0.3em; border-radius: 3px; font-family: var(--vscode-editor-font-family); }
      .cta { display: inline-block; padding: 0.5rem 0.9rem; border: 0; border-radius: 4px; background: var(--vscode-button-background); color: var(--vscode-button-foreground); cursor: pointer; text-decoration: none; font: inherit; }
      .cta:hover { background: var(--vscode-button-hoverBackground); }
      .meta { margin-top: 1rem; font-size: 0.85rem; color: var(--vscode-descriptionForeground); }
    </style>
  </head>
  <body>
    <div class="wrap">
      <div class="card">
        <h1>Observer Dashboard</h1>
        <p>${hint}</p>
        ${cta}
        <p class="meta">Mode: <code>${state.mode}</code> · Dashboard port: <code>${state.dashboardPort}</code> · Proxy port: <code>${state.proxyPort}</code></p>
      </div>
    </div>
    <script nonce="${nonce}">
      const vscode = acquireVsCodeApi();
      const btn = document.getElementById('start-btn');
      if (btn) {
        btn.addEventListener('click', () => {
          btn.disabled = true;
          btn.textContent = 'Starting…';
          vscode.postMessage({ cmd: 'startDaemon' });
        });
      }
    </script>
  </body>
</html>`;
}
