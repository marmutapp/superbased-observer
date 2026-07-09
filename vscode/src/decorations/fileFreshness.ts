// FileDecorationProvider + HoverProvider for per-file observer
// freshness. Standard async-provider pattern: peek the cache
// synchronously; on miss, return undefined and kick a background
// fetch that fires _onDidChangeFileDecorations(uri) when it lands so
// VS Code re-asks.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import { output } from '../output';
import { FileFreshnessCache } from './cache';
import { decorationBadge, decorationTooltip, hoverMarkdown } from './format';

const DECORATION_COLOR = new vscode.ThemeColor('charts.blue');

export class FileFreshnessProvider
  implements vscode.FileDecorationProvider, vscode.HoverProvider, vscode.Disposable
{
  private readonly _onDidChangeFileDecorations = new vscode.EventEmitter<
    vscode.Uri | vscode.Uri[]
  >();
  readonly onDidChangeFileDecorations = this._onDidChangeFileDecorations.event;

  private readonly cache: FileFreshnessCache;
  private readonly disposables: vscode.Disposable[] = [];

  constructor(private readonly daemon: DaemonManager) {
    this.cache = new FileFreshnessCache((p) => this.daemon.getClient().fileState(p));
    this.disposables.push(
      this._onDidChangeFileDecorations,
      this.daemon.onDidChangeState((s) => {
        if (s.status === 'idle') this.cache.clear();
        this._onDidChangeFileDecorations.fire([]);
      }),
    );
  }

  provideFileDecoration(uri: vscode.Uri): vscode.FileDecoration | undefined {
    if (!this.shouldDecorate(uri)) return undefined;
    const cached = this.cache.peek(uri.fsPath);
    if (!cached) {
      void this.kickFetch(uri);
      return undefined;
    }
    const badge = decorationBadge(cached);
    if (!badge) return undefined;
    return {
      badge,
      tooltip: `Observer: ${decorationTooltip(cached)}`,
      color: DECORATION_COLOR,
      propagate: false,
    };
  }

  async provideHover(
    document: vscode.TextDocument,
    position: vscode.Position,
  ): Promise<vscode.Hover | undefined> {
    if (position.line !== 0) return undefined;
    if (!this.shouldDecorate(document.uri)) return undefined;
    let state = this.cache.peek(document.uri.fsPath);
    if (!state) {
      try {
        state = await this.cache.get(document.uri.fsPath);
      } catch (err) {
        output.appendLine(`Hover fetch failed: ${(err as Error).message}`);
        return undefined;
      }
    }
    const md = new vscode.MarkdownString(hoverMarkdown(state));
    md.supportThemeIcons = true;
    return new vscode.Hover(md);
  }

  private async kickFetch(uri: vscode.Uri): Promise<void> {
    try {
      await this.cache.get(uri.fsPath);
      this._onDidChangeFileDecorations.fire(uri);
    } catch (err) {
      output.appendLine(
        `File freshness fetch failed for ${uri.fsPath}: ${(err as Error).message}`,
      );
    }
  }

  /**
   * Skip files outside the workspace + non-file-scheme URIs. Without
   * this the explorer's git decorations would trigger /api calls for
   * every system file VS Code lists, which would slam the daemon.
   */
  private shouldDecorate(uri: vscode.Uri): boolean {
    if (uri.scheme !== 'file') return false;
    if (this.daemon.getState().status === 'idle') return false;
    const folder = vscode.workspace.getWorkspaceFolder(uri);
    return folder !== undefined;
  }

  dispose(): void {
    while (this.disposables.length) {
      try {
        this.disposables.pop()?.dispose();
      } catch {
        /* best effort */
      }
    }
    this.cache.clear();
  }
}

export function registerFileFreshness(
  ctx: vscode.ExtensionContext,
  daemon: DaemonManager,
): void {
  const provider = new FileFreshnessProvider(daemon);
  ctx.subscriptions.push(
    provider,
    vscode.window.registerFileDecorationProvider(provider),
    vscode.languages.registerHoverProvider({ scheme: 'file' }, provider),
  );
}
