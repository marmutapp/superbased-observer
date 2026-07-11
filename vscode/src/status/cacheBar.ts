// Cache-expiry status bar item.
//
// Polls /api/cache/status every 15s. Shows a countdown to the most urgent
// live prompt cache's expiry ("$(database) cache 1:12") so the operator
// can keep a valuable cache warm before paying a cold re-write; turns to a
// warning state when a high-value cache is critical or already cold. The
// tooltip lists the at-risk caches + the keep-warm recommendation (e.g.
// switch to the 1h tier). Hidden entirely when nothing is at risk, so it
// adds no noise during normal warm operation. Click → observer.openDashboard.
//
// Part A3 of docs/plans/cache-expiry-warning-and-keepwarm-plan-2026-06-25.md.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import type { CacheStatusResponse, CacheWindowStatus } from '../api/types';
import { output } from '../output';

const POLL_MS = 15_000;
const FIRST_PROBE_DELAY_MS = 1_500;

export interface StatusBarController extends vscode.Disposable {
  refresh(): Promise<void>;
}

export function createCacheStatusBar(
  ctx: vscode.ExtensionContext,
  daemon: DaemonManager,
): StatusBarController | undefined {
  const cfg = vscode.workspace.getConfiguration('observer');
  if (!cfg.get<boolean>('cacheStatusBar.enabled', true)) {
    output.appendLine('Cache status bar disabled by observer.cacheStatusBar.enabled');
    return undefined;
  }

  // Sits just left of the cost item (priority 99 < 100).
  const item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 99);
  item.name = 'Observer: Cache expiry';
  item.command = 'observer.openDashboard';
  item.hide();
  ctx.subscriptions.push(item);

  let timer: NodeJS.Timeout | undefined;
  let disposed = false;

  const refresh = async (): Promise<void> => {
    if (disposed) return;
    const state = daemon.getState();
    if (state.status === 'idle') {
      item.hide();
      return;
    }
    try {
      const data = await daemon.getClient().cacheStatus();
      render(item, data);
    } catch (err) {
      output.appendLine(`Cache status poll failed: ${(err as Error).message}`);
      item.hide(); // a cost bar already surfaces daemon trouble; stay quiet here
    }
  };

  const startTimer = (): void => {
    timer = setTimeout(async function tick() {
      await refresh();
      if (!disposed) timer = setTimeout(tick, POLL_MS);
    }, FIRST_PROBE_DELAY_MS);
  };

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

function render(item: vscode.StatusBarItem, data: CacheStatusResponse): void {
  if (!data.enabled) {
    item.hide();
    return;
  }
  const windows = (data.windows ?? []).filter((w) => w.severity !== 'ok');
  if (windows.length === 0) {
    item.hide();
    return;
  }
  // windows arrive most-urgent-first.
  const top = windows[0];
  item.text = `$(database) ${labelFor(top)}`;
  item.tooltip = buildTooltip(data.keepwarm_mode, windows);
  item.backgroundColor =
    top.severity === 'critical' || top.severity === 'cold'
      ? new vscode.ThemeColor('statusBarItem.warningBackground')
      : undefined;
  item.show();
}

function labelFor(w: CacheWindowStatus): string {
  if (w.severity === 'cold') return 'cache cold';
  return `cache ${countdown(w.seconds_to_expiry, w.estimated)}`;
}

function countdown(secs: number, estimated: boolean): string {
  const prefix = estimated ? '~' : '';
  if (secs <= 0) return 'cold';
  if (secs >= 60) {
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return `${prefix}${m}:${s.toString().padStart(2, '0')}`;
  }
  return `${prefix}${secs}s`;
}

function buildTooltip(mode: string, windows: CacheWindowStatus[]): vscode.MarkdownString {
  const md = new vscode.MarkdownString();
  md.supportThemeIcons = true;
  const lines: string[] = ['**Prompt caches at risk**', ''];
  for (const w of windows.slice(0, 6)) {
    const when = w.severity === 'cold' ? 'cold' : countdown(w.seconds_to_expiry, w.estimated);
    lines.push(
      `- ${sev(w.severity)} **${w.window.model}** — ${when}, ${formatUSD(
        w.value_at_risk_usd,
      )} at risk`,
    );
    if (mode !== 'off' && w.recommendation.action !== 'none') {
      lines.push(`  - 💡 ${w.recommendation.rationale}`);
    }
  }
  lines.push('', '_Click to open the dashboard._');
  md.appendMarkdown(lines.join('\n'));
  return md;
}

function sev(s: string): string {
  switch (s) {
    case 'cold':
      return '$(error)';
    case 'critical':
      return '$(warning)';
    default:
      return '$(clock)';
  }
}

function formatUSD(n: number): string {
  if (!Number.isFinite(n)) return '$—';
  return `$${n.toFixed(2)}`;
}
