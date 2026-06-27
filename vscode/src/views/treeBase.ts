// Shared scaffolding for the four sidebar TreeDataProviders.
//
// Each provider stores its current "data slot" (live | empty |
// error | idle), and emits a refresh event when it changes. The
// TreePoller drives the refresh cadence per the documented per-view
// schedule in docs/vscode-extension-tracker.md M3.

import * as vscode from 'vscode';

export type TreeStatus =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'live' }
  | { kind: 'empty' }
  | { kind: 'error'; message: string };

export class TreePoller implements vscode.Disposable {
  private timer: NodeJS.Timeout | undefined;
  private disposed = false;
  constructor(
    private readonly fn: () => Promise<void>,
    private readonly intervalMs: number,
    private readonly firstProbeMs = 100,
  ) {}

  start(): void {
    const tick = async (): Promise<void> => {
      if (this.disposed) return;
      try {
        await this.fn();
      } catch {
        /* errors are surfaced via the provider's status */
      }
      if (!this.disposed) {
        this.timer = setTimeout(tick, this.intervalMs);
      }
    };
    this.timer = setTimeout(tick, this.firstProbeMs);
  }

  dispose(): void {
    this.disposed = true;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
  }
}

/**
 * makePlaceholder builds a TreeItem for the idle / empty / error
 * branches. The label carries the message; description carries the
 * status keyword; viewItem is empty so no context menu fires.
 */
export function makePlaceholder(
  label: string,
  options: { command?: vscode.Command; tooltip?: string } = {},
): vscode.TreeItem {
  const item = new vscode.TreeItem(label, vscode.TreeItemCollapsibleState.None);
  if (options.command) item.command = options.command;
  if (options.tooltip) item.tooltip = options.tooltip;
  return item;
}
