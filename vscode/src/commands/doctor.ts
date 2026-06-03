import * as vscode from 'vscode';
import type { ResolvedBinary } from '../binary';

export function registerDoctor(
  ctx: vscode.ExtensionContext,
  bin: ResolvedBinary,
): vscode.Disposable {
  return vscode.commands.registerCommand('observer.doctor', () => {
    const term = vscode.window.createTerminal({ name: 'Observer Doctor' });
    const quoted = bin.path.includes(' ') ? `"${bin.path}"` : bin.path;
    term.sendText(`${quoted} doctor`);
    term.show();
    ctx.subscriptions.push(term);
  });
}
