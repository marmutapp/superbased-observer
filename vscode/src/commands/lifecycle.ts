import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import { DashboardPanel } from '../webview/dashboard';

export function registerLifecycle(
  ctx: vscode.ExtensionContext,
  daemon: DaemonManager,
): vscode.Disposable[] {
  const disposables: vscode.Disposable[] = [
    vscode.commands.registerCommand('observer.start', async () => {
      await daemon.start();
    }),
    vscode.commands.registerCommand('observer.stop', async () => {
      await daemon.stop();
    }),
    vscode.commands.registerCommand('observer.openDashboard', () => {
      DashboardPanel.createOrShow(daemon);
    }),
  ];
  ctx.subscriptions.push(...disposables);
  return disposables;
}
