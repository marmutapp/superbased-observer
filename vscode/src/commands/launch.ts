import * as vscode from 'vscode';
import type { ResolvedBinary } from '../binary';

// "Launch <tool> (proxied)" — L2a of the usability arc (P4.5): an
// integrated terminal running the observer wrapper, which sets
// ANTHROPIC_BASE_URL (and re-exports a FRESH Pro/Max OAuth token —
// stale tokens are deliberately skipped since D13, so the session
// never 401s on a dead re-export). The terminal is the honest
// mechanism: the daemon can't host an interactive session, and the
// user sees exactly what ran.
export function registerLaunch(
  ctx: vscode.ExtensionContext,
  bin: ResolvedBinary,
): void {
  const quoted = bin.path.includes(' ') ? `"${bin.path}"` : bin.path;
  const launch = (id: string, name: string, sub: string) =>
    vscode.commands.registerCommand(id, () => {
      const term = vscode.window.createTerminal({
        name,
        cwd: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath,
      });
      term.sendText(`${quoted} ${sub}`);
      term.show();
      ctx.subscriptions.push(term);
    });
  ctx.subscriptions.push(
    launch('observer.launchClaude', 'Claude Code (proxied)', 'claude'),
    launch('observer.launchCodex', 'Codex (proxied)', 'codex'),
  );
}
