// Watcher-lag notification controller. Polls /api/health/watcher
// every 30s; fires when behind_total_bytes > 10 KB, deduped per
// worst-file path on a 5-min window.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import { output } from '../output';
import { decideWatcherLag } from './decide';

const POLL_MS = 30 * 1000;
const FIRST_PROBE_MS = 5 * 1000;

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)} MB`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)} kB`;
  return `${n} B`;
}

function baseName(p: string): string {
  if (!p) return p;
  const cleaned = p.replace(/[\\/]+$/, '');
  const idx = Math.max(cleaned.lastIndexOf('/'), cleaned.lastIndexOf('\\'));
  return idx >= 0 ? cleaned.slice(idx + 1) : cleaned;
}

export class WatcherLagNotifier implements vscode.Disposable {
  private timer: NodeJS.Timeout | undefined;
  private disposed = false;
  private readonly lastFiredAtMs = new Map<string, number>();
  private readonly disposables: vscode.Disposable[] = [];

  constructor(private readonly daemon: DaemonManager) {
    this.disposables.push(
      this.daemon.onDidChangeState(() => {
        if (this.daemon.getState().status === 'idle') this.lastFiredAtMs.clear();
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

  private async poll(): Promise<void> {
    if (this.daemon.getState().status === 'idle') return;
    try {
      const data = await this.daemon.getClient().watcherHealth();
      const decision = decideWatcherLag(data, this.lastFiredAtMs, Date.now());
      if (!decision.fire) return;
      this.lastFiredAtMs.set(decision.worstFile!, Date.now());
      const msg =
        `Observer watcher behind on ${baseName(decision.worstFile!)} ` +
        `(${formatBytes(decision.worstBytes ?? 0)}; total lag ${formatBytes(decision.totalBytes ?? 0)}).`;
      const choice = await vscode.window.showWarningMessage(msg, 'Show Output', 'Dismiss');
      if (choice === 'Show Output') {
        void vscode.commands.executeCommand('observer.showOutput');
      }
    } catch (err) {
      output.appendLine(`Watcher-lag poll failed: ${(err as Error).message}`);
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
