// Today-spend status bar item.
//
// Polls /api/analysis/headline?days=1 every 60s. Shows period.cost_usd
// with delta vs yesterday in the tooltip alongside the top model and
// month projection. Click → observer.openDashboard. Degrades to a
// "Observer not running" state when the daemon is unreachable; in
// managed/auto mode the click action upgrades to "Restart Daemon"
// so the operator can recover without leaving the editor.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import type { HeadlineResponse } from '../api/types';
import { output } from '../output';

const POLL_MS = 60_000;
const FIRST_PROBE_DELAY_MS = 1_000;

export interface StatusBarController extends vscode.Disposable {
  refresh(): Promise<void>;
}

export function createCostStatusBar(
  ctx: vscode.ExtensionContext,
  daemon: DaemonManager,
): StatusBarController | undefined {
  const cfg = vscode.workspace.getConfiguration('observer');
  if (!cfg.get<boolean>('statusBar.enabled', true)) {
    output.appendLine('Status bar disabled by observer.statusBar.enabled');
    return undefined;
  }

  const item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
  item.name = 'Observer: Today spend';
  item.command = 'observer.openDashboard';
  item.text = '$(graph) Observer …';
  item.tooltip = 'Observer: polling…';
  item.show();
  ctx.subscriptions.push(item);

  let timer: NodeJS.Timeout | undefined;
  let disposed = false;

  const refresh = async (): Promise<void> => {
    if (disposed) return;
    const state = daemon.getState();
    if (state.status === 'idle') {
      renderIdle(item, state.mode);
      return;
    }
    try {
      const data = await daemon.getClient().headline(1);
      renderHeadline(item, data);
    } catch (err) {
      output.appendLine(`Status bar poll failed: ${(err as Error).message}`);
      renderDegraded(item, state.mode);
    }
  };

  const startTimer = (): void => {
    timer = setTimeout(async function tick() {
      await refresh();
      if (!disposed) timer = setTimeout(tick, POLL_MS);
    }, FIRST_PROBE_DELAY_MS);
  };

  // Repaint on every daemon state transition so we don't wait up to
  // a full 60s after a start/stop to reflect the new reality.
  ctx.subscriptions.push(daemon.onDidChangeState(() => void refresh()));

  startTimer();

  return {
    refresh,
    dispose: () => {
      disposed = true;
      if (timer) clearTimeout(timer);
      item.dispose();
    },
  };
}

function renderHeadline(item: vscode.StatusBarItem, data: HeadlineResponse): void {
  const spend = data.period?.cost_usd ?? 0;
  item.text = `$(graph) ${formatUSD(spend)}`;
  item.tooltip = buildTooltip(data);
  item.backgroundColor = undefined;
}

function renderIdle(item: vscode.StatusBarItem, mode: string): void {
  item.text = '$(circle-slash) Observer idle';
  const hint =
    mode === 'detect'
      ? 'No daemon running. Run `observer start` in a terminal, or set `observer.daemon.mode` to `managed`.'
      : 'No daemon running. Run `Observer: Start Daemon` to launch one.';
  const md = new vscode.MarkdownString();
  md.appendMarkdown(`**Observer**: idle\n\n${hint}`);
  item.tooltip = md;
  item.backgroundColor = new vscode.ThemeColor('statusBarItem.warningBackground');
}

function renderDegraded(item: vscode.StatusBarItem, mode: string): void {
  item.text = '$(warning) Observer unreachable';
  const md = new vscode.MarkdownString();
  md.appendMarkdown(
    `**Observer**: dashboard did not respond.\n\n` +
      (mode === 'detect'
        ? 'The lockfile says a daemon is running, but its dashboard port is not answering. Check the Output channel.'
        : 'Click to restart the extension-managed daemon.'),
  );
  item.tooltip = md;
  item.backgroundColor = new vscode.ThemeColor('statusBarItem.errorBackground');
}

function buildTooltip(data: HeadlineResponse): vscode.MarkdownString {
  const md = new vscode.MarkdownString();
  md.supportThemeIcons = true;
  const spend = data.period?.cost_usd ?? 0;
  const delta = data.period?.delta_pct ?? 0;
  const top = data.top_model?.key ?? 'unknown';
  const topShare = data.top_model?.concentration_pct ?? 0;
  const burn = data.burn_rate?.cost_per_hour_usd ?? 0;
  const month = data.month;
  const lines = [
    `**Today's spend**: ${formatUSD(spend)} (${formatPct(delta)} vs yesterday)`,
    `**Top model**: ${top} (${topShare.toFixed(1)}% share)`,
    `**Burn rate**: ${formatUSD(burn)}/hr`,
  ];
  if (month) {
    const budget =
      month.budget_usd > 0
        ? ` — budget ${month.budget_pct.toFixed(1)}% of ${formatUSD(month.budget_usd)}`
        : '';
    lines.push(
      `**Month**: ${formatUSD(month.to_date_usd)} to date — projection ${formatUSD(
        month.projection_usd,
      )}${budget}`,
    );
  }
  lines.push('', '_Click to open the dashboard._');
  md.appendMarkdown(lines.join('\n\n'));
  return md;
}

function formatUSD(n: number): string {
  if (!Number.isFinite(n)) return '$—';
  if (Math.abs(n) >= 1000) {
    return `$${n.toLocaleString('en-US', { maximumFractionDigits: 0 })}`;
  }
  return `$${n.toFixed(2)}`;
}

function formatPct(n: number): string {
  if (!Number.isFinite(n)) return '—';
  const sign = n >= 0 ? '+' : '';
  return `${sign}${n.toFixed(1)}%`;
}
