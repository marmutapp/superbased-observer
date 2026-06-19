// Discovery TreeView — backed by /api/discover.
//
// Shows the top cross-tool files (files touched by 2+ tools) so the
// operator can spot overlap. Refreshes every 5 min.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import type { DiscoverCrossToolFile } from '../api/types';
import { output } from '../output';
import { basename } from './format';
import { TreePoller, TreeStatus, makePlaceholder } from './treeBase';

const POLL_MS = 5 * 60_000;
const TOP_N = 20;

export class DiscoveryFileItem extends vscode.TreeItem {
  constructor(public readonly file: DiscoverCrossToolFile) {
    super(basename(file.file_path) || file.file_path, vscode.TreeItemCollapsibleState.None);
    this.description = `${file.tools.join(', ')} · ${file.accesses}×`;
    this.tooltip = buildTooltip(file);
    this.contextValue = 'discoveryFile';
    this.iconPath = new vscode.ThemeIcon('file');
  }
}

export class DiscoveryTreeProvider implements vscode.TreeDataProvider<vscode.TreeItem> {
  private readonly _onDidChangeTreeData = new vscode.EventEmitter<undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;
  private status: TreeStatus = { kind: 'loading' };
  private rows: DiscoverCrossToolFile[] = [];
  private readonly poller: TreePoller;

  constructor(private readonly daemon: DaemonManager) {
    this.poller = new TreePoller(() => this.refreshNow(), POLL_MS);
  }

  start(ctx: vscode.ExtensionContext): void {
    this.poller.start();
    ctx.subscriptions.push(
      this.poller,
      this.daemon.onDidChangeState(() => this.refresh()),
    );
  }

  refresh(): void {
    void this.refreshNow();
  }

  private async refreshNow(): Promise<void> {
    const state = this.daemon.getState();
    if (state.status === 'idle') {
      this.status = { kind: 'idle' };
      this.rows = [];
      this.fire();
      return;
    }
    try {
      const res = await this.daemon.getClient().discover();
      const all = Array.isArray(res.cross_tool_files) ? res.cross_tool_files : [];
      this.rows = all.slice(0, TOP_N);
      this.status = this.rows.length === 0 ? { kind: 'empty' } : { kind: 'live' };
      this.fire();
    } catch (err) {
      output.appendLine(`Discovery tree poll failed: ${(err as Error).message}`);
      this.status = { kind: 'error', message: (err as Error).message };
      this.fire();
    }
  }

  private fire(): void {
    this._onDidChangeTreeData.fire(undefined);
  }

  getTreeItem(element: vscode.TreeItem): vscode.TreeItem {
    return element;
  }

  getChildren(): vscode.TreeItem[] {
    switch (this.status.kind) {
      case 'idle':
        return [
          makePlaceholder('Daemon not running', {
            command: { command: 'observer.start', title: 'Start Daemon' },
          }),
        ];
      case 'loading':
        return [makePlaceholder('Loading…')];
      case 'error':
        return [
          makePlaceholder('Failed to load — check Output channel', {
            tooltip: this.status.message,
          }),
        ];
      case 'empty':
        return [makePlaceholder('No cross-tool overlap detected yet')];
      case 'live':
        return this.rows.map((file) => new DiscoveryFileItem(file));
    }
  }
}

function buildTooltip(file: DiscoverCrossToolFile): vscode.MarkdownString {
  const md = new vscode.MarkdownString();
  const lines = [
    `**${file.file_path}**`,
    `**Project**: ${file.project || '(none)'}`,
    `**Tools**: ${file.tools.join(', ')}`,
    `**Accesses**: ${file.accesses}`,
  ];
  md.appendMarkdown(lines.join('\n\n'));
  return md;
}
