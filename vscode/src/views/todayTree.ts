// Today TreeView — backed by /api/analysis/headline?days=1.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import type { HeadlineResponse } from '../api/types';
import { output } from '../output';
import { formatPct, formatUSD } from './format';
import { TreePoller, TreeStatus, makePlaceholder } from './treeBase';

const POLL_MS = 60_000;

export class TodayTreeProvider implements vscode.TreeDataProvider<vscode.TreeItem> {
  private readonly _onDidChangeTreeData = new vscode.EventEmitter<undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;
  private status: TreeStatus = { kind: 'loading' };
  private headline?: HeadlineResponse;
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

  /** External, fire-and-forget. Used by the refresh command. */
  refresh(): void {
    void this.refreshNow();
  }

  private async refreshNow(): Promise<void> {
    const state = this.daemon.getState();
    if (state.status === 'idle') {
      this.status = { kind: 'idle' };
      this.headline = undefined;
      this.fire();
      return;
    }
    try {
      this.headline = await this.daemon.getClient().headline(1);
      this.status = { kind: 'live' };
      this.fire();
    } catch (err) {
      output.appendLine(`Today tree poll failed: ${(err as Error).message}`);
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
            command: {
              command: 'observer.start',
              title: 'Start Daemon',
            },
          }),
        ];
      case 'loading':
        return [makePlaceholder('Loading…')];
      case 'error':
        return [
          makePlaceholder('Failed to load — check Output channel', {
            tooltip: this.status.message,
            command: { command: 'observer.showOutput', title: 'Show Output' },
          }),
        ];
      case 'live':
        return this.headline ? renderHeadline(this.headline) : [];
      case 'empty':
        return [makePlaceholder('No activity today yet')];
    }
  }
}

function renderHeadline(data: HeadlineResponse): vscode.TreeItem[] {
  const spend = data.period?.cost_usd ?? 0;
  const delta = data.period?.delta_pct ?? 0;
  const items: vscode.TreeItem[] = [
    pair('Spend', formatUSD(spend), `${formatPct(delta)} vs yesterday`),
    pair(
      'Top model',
      data.top_model?.key ?? 'unknown',
      `${(data.top_model?.concentration_pct ?? 0).toFixed(1)}% share`,
    ),
    pair('Burn rate', `${formatUSD(data.burn_rate?.cost_per_hour_usd ?? 0)}/hr`),
  ];
  if (data.month) {
    items.push(
      pair(
        'Month to date',
        formatUSD(data.month.to_date_usd),
        `projection ${formatUSD(data.month.projection_usd)}`,
      ),
    );
  }
  return items;
}

function pair(label: string, value: string, description?: string): vscode.TreeItem {
  const item = new vscode.TreeItem(`${label}: ${value}`, vscode.TreeItemCollapsibleState.None);
  if (description) item.description = description;
  return item;
}
