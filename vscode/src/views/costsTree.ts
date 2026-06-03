// Costs TreeView — backed by /api/cost?days=7&group-by=model.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import type { CostRow } from '../api/types';
import { output } from '../output';
import { formatTokens, formatUSD } from './format';
import { TreePoller, TreeStatus, makePlaceholder } from './treeBase';

const POLL_MS = 5 * 60_000;
const DAYS = 7;

export class CostRowItem extends vscode.TreeItem {
  constructor(public readonly row: CostRow) {
    super(row.key, vscode.TreeItemCollapsibleState.None);
    this.description = `${formatUSD(row.cost_usd)} · ${row.turn_count} turns`;
    this.tooltip = buildTooltip(row);
    this.contextValue = 'costRow';
    this.iconPath = new vscode.ThemeIcon('credit-card');
  }
}

export class CostsTreeProvider implements vscode.TreeDataProvider<vscode.TreeItem> {
  private readonly _onDidChangeTreeData = new vscode.EventEmitter<undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;
  private status: TreeStatus = { kind: 'loading' };
  private rows: CostRow[] = [];
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
      const res = await this.daemon.getClient().cost(DAYS, 'model');
      const all = Array.isArray(res.rows) ? res.rows : [];
      // Already pre-sorted by spend on the Go side; just take the
      // top of the list.
      this.rows = all.slice(0, 12);
      this.status = this.rows.length === 0 ? { kind: 'empty' } : { kind: 'live' };
      this.fire();
    } catch (err) {
      output.appendLine(`Costs tree poll failed: ${(err as Error).message}`);
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
        return [makePlaceholder('No cost data in the last 7 days')];
      case 'live':
        return this.rows.map((row) => new CostRowItem(row));
    }
  }
}

function buildTooltip(row: CostRow): vscode.MarkdownString {
  const md = new vscode.MarkdownString();
  const lines = [
    `**${row.key}**`,
    `**Cost**: ${formatUSD(row.cost_usd)} (${row.reliability})`,
    `**Turns**: ${row.turn_count}`,
    `**Input tokens**: ${formatTokens(row.tokens?.input ?? 0)}`,
    `**Output tokens**: ${formatTokens(row.tokens?.output ?? 0)}`,
    `**Cache read**: ${formatTokens(row.tokens?.cache_read ?? 0)}`,
  ];
  if ((row.tokens?.reasoning ?? 0) > 0) {
    lines.push(`**Reasoning**: ${formatTokens(row.tokens.reasoning)}`);
  }
  md.appendMarkdown(lines.join('\n\n'));
  return md;
}
