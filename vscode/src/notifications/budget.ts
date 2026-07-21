// Budget-breach notification controller. Polls
// /api/analysis/headline?days=1 once per hour; fires when
// month.budget_usd > 0 AND month.budget_pct >= 80, deduped to once
// per UTC calendar day via globalState.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import { output } from '../output';
import { decideBudget } from './decide';

const POLL_MS = 60 * 60 * 1000; // hourly
const FIRST_PROBE_MS = 30 * 1000; // wait 30s before first poll so activation isn't noisy
const STATE_KEY = 'observer.budget.lastFiredDay';

export class BudgetNotifier implements vscode.Disposable {
  private timer: NodeJS.Timeout | undefined;
  private disposed = false;
  private readonly disposables: vscode.Disposable[] = [];

  constructor(
    private readonly ctx: vscode.ExtensionContext,
    private readonly daemon: DaemonManager,
  ) {
    this.disposables.push(
      this.daemon.onDidChangeState(() => {
        if (this.daemon.getState().status !== 'idle') this.kick();
      }),
    );
  }

  start(): void {
    this.timer = setTimeout(() => this.tick(), FIRST_PROBE_MS);
  }

  private async tick(): Promise<void> {
    if (this.disposed) return;
    await this.poll();
    if (!this.disposed) {
      this.timer = setTimeout(() => this.tick(), POLL_MS);
    }
  }

  /** External, fire-and-forget; used on state transitions. */
  private kick(): void {
    void this.poll();
  }

  private async poll(): Promise<void> {
    if (this.daemon.getState().status === 'idle') return;
    try {
      const data = await this.daemon.getClient().headline(1);
      const lastFiredDay = this.ctx.globalState.get<string>(STATE_KEY);
      const decision = decideBudget(data, lastFiredDay, new Date());
      if (!decision.fire) return;
      const today = new Date().toISOString().slice(0, 10);
      await this.ctx.globalState.update(STATE_KEY, today);
      const msg =
        `Observer: monthly budget at ${decision.pct!.toFixed(0)}% ` +
        `(${formatUSD(decision.toDateUsd ?? 0)} / ${formatUSD(decision.budgetUsd!)}).`;
      const choice = await vscode.window.showWarningMessage(msg, 'Open Dashboard', 'Dismiss');
      if (choice === 'Open Dashboard') {
        void vscode.commands.executeCommand('observer.openDashboard');
      }
    } catch (err) {
      output.appendLine(`Budget poll failed: ${(err as Error).message}`);
    }
  }

  dispose(): void {
    this.disposed = true;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
    while (this.disposables.length) {
      try {
        this.disposables.pop()?.dispose();
      } catch {
        /* best effort */
      }
    }
  }
}

function formatUSD(n: number): string {
  if (!Number.isFinite(n)) return '$—';
  if (Math.abs(n) >= 1000) {
    return `$${n.toLocaleString('en-US', { maximumFractionDigits: 0 })}`;
  }
  return `$${n.toFixed(2)}`;
}
