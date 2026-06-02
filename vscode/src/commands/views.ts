// Sidebar tree commands.
//
// observer.refreshTrees forces a refresh on all four providers at
// once. observer.openSession copies the session id to the clipboard
// (no web/src ?session= adapter exists yet — captured as deferred
// M3 follow-up in the tracker) then opens the dashboard panel.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import { DashboardPanel } from '../webview/dashboard';
import type { SessionItem } from '../views/sessionsTree';
import type { DiscoveryFileItem } from '../views/discoveryTree';
import type { TodayTreeProvider } from '../views/todayTree';
import type { SessionsTreeProvider } from '../views/sessionsTree';
import type { DiscoveryTreeProvider } from '../views/discoveryTree';
import type { CostsTreeProvider } from '../views/costsTree';

export interface TreeProviders {
  today: TodayTreeProvider;
  sessions: SessionsTreeProvider;
  discovery: DiscoveryTreeProvider;
  costs: CostsTreeProvider;
}

export function registerViewCommands(
  ctx: vscode.ExtensionContext,
  daemon: DaemonManager,
  trees: TreeProviders,
): void {
  ctx.subscriptions.push(
    vscode.commands.registerCommand('observer.refreshTrees', () => {
      trees.today.refresh();
      trees.sessions.refresh();
      trees.discovery.refresh();
      trees.costs.refresh();
    }),
    vscode.commands.registerCommand('observer.openSession', async (item?: SessionItem) => {
      const id = item?.row?.id;
      if (id) {
        await vscode.env.clipboard.writeText(id);
        void vscode.window.showInformationMessage(
          `Session ID copied (${id.slice(0, 8)}…). Paste it into the dashboard's session filter.`,
        );
      }
      DashboardPanel.createOrShow(daemon);
    }),
    vscode.commands.registerCommand('observer.copySessionId', async (item?: SessionItem) => {
      const id = item?.row?.id;
      if (!id) return;
      await vscode.env.clipboard.writeText(id);
      void vscode.window.showInformationMessage(`Copied session ID: ${id}`);
    }),
    vscode.commands.registerCommand('observer.copyPath', async (item?: DiscoveryFileItem) => {
      const p = item?.file?.file_path;
      if (!p) return;
      await vscode.env.clipboard.writeText(p);
      void vscode.window.showInformationMessage(`Copied path: ${p}`);
    }),
    // Internal hook used by tree error placeholders.
    vscode.commands.registerCommand('observer.showOutput', () => {
      void vscode.commands.executeCommand('workbench.action.output.show.observer');
    }),
  );
}
