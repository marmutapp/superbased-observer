// observer.refreshInstructions + observer.previewInstructions —
// wired from the InstructionFilesCodeLensProvider.
//
// Both spawn `<bin> suggest` with the relevant --target. Refresh adds
// --apply and triggers workbench.action.files.revert so the editor
// surfaces the new content without a "file changed on disk" race.
// Preview captures stdout (default dry-run) and opens it as a new
// untitled markdown editor beside the original.

import * as vscode from 'vscode';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import type { ResolvedBinary } from '../binary';
import type { InstructionTarget } from '../codelens/targets';
import { output } from '../output';

const exec = promisify(execFile);

export function registerInstructionCommands(
  ctx: vscode.ExtensionContext,
  bin: ResolvedBinary,
): void {
  ctx.subscriptions.push(
    vscode.commands.registerCommand(
      'observer.refreshInstructions',
      async (fileUri?: vscode.Uri, target?: InstructionTarget) => {
        const ctxPair = await resolveContext(fileUri, target);
        if (!ctxPair) return;
        const { workspaceRoot, target: t, uri } = ctxPair;
        try {
          await runSuggest(bin.path, workspaceRoot, t, true);
          await vscode.window.showTextDocument(uri, { preview: false });
          await vscode.commands.executeCommand('workbench.action.files.revert');
          void vscode.window.showInformationMessage(
            `Observer: refreshed ${baseName(uri.fsPath)} from learnings.`,
          );
        } catch (err) {
          surfaceError(err, `refresh ${baseName(uri.fsPath)}`);
        }
      },
    ),
    vscode.commands.registerCommand(
      'observer.previewInstructions',
      async (fileUri?: vscode.Uri, target?: InstructionTarget) => {
        const ctxPair = await resolveContext(fileUri, target);
        if (!ctxPair) return;
        const { workspaceRoot, target: t, uri } = ctxPair;
        try {
          const stdout = await runSuggest(bin.path, workspaceRoot, t, false);
          const preview = await vscode.workspace.openTextDocument({
            language: 'markdown',
            content: stdout,
          });
          await vscode.window.showTextDocument(preview, {
            viewColumn: vscode.ViewColumn.Beside,
            preview: true,
          });
        } catch (err) {
          surfaceError(err, `preview suggestions for ${baseName(uri.fsPath)}`);
        }
      },
    ),
  );
}

interface ResolvedCtx {
  uri: vscode.Uri;
  workspaceRoot: string;
  target: InstructionTarget;
}

async function resolveContext(
  fileUri: vscode.Uri | undefined,
  target: InstructionTarget | undefined,
): Promise<ResolvedCtx | undefined> {
  const uri = fileUri ?? vscode.window.activeTextEditor?.document.uri;
  if (!uri) {
    void vscode.window.showErrorMessage('Observer: no file context for this command.');
    return undefined;
  }
  const folder = vscode.workspace.getWorkspaceFolder(uri);
  if (!folder) {
    void vscode.window.showErrorMessage(
      `Observer: ${baseName(uri.fsPath)} is not inside any workspace folder; --project is required.`,
    );
    return undefined;
  }
  if (!target) {
    void vscode.window.showErrorMessage(
      `Observer: could not classify ${baseName(uri.fsPath)} (expected CLAUDE.md / AGENTS.md / .cursorrules).`,
    );
    return undefined;
  }
  return { uri, workspaceRoot: folder.uri.fsPath, target };
}

async function runSuggest(
  binPath: string,
  workspaceRoot: string,
  target: InstructionTarget,
  apply: boolean,
): Promise<string> {
  const args = ['suggest', '--project', workspaceRoot, '--target', target];
  if (apply) args.push('--apply');
  output.appendLine(`Running ${binPath} ${args.join(' ')}`);
  const { stdout } = await exec(binPath, args, {
    timeout: 30_000,
    maxBuffer: 4 * 1024 * 1024,
  });
  return stdout;
}

function baseName(p: string): string {
  if (!p) return p;
  const cleaned = p.replace(/[\\/]+$/, '');
  const idx = Math.max(cleaned.lastIndexOf('/'), cleaned.lastIndexOf('\\'));
  return idx >= 0 ? cleaned.slice(idx + 1) : cleaned;
}

function surfaceError(err: unknown, what: string): void {
  const message = err instanceof Error ? err.message : String(err);
  output.appendLine(`Failed to ${what}: ${message}`);
  void vscode.window.showErrorMessage(`Observer: failed to ${what} — ${shortError(message)}`);
}

function shortError(message: string): string {
  const firstLine = message.split('\n', 1)[0] ?? message;
  return firstLine.length > 200 ? firstLine.slice(0, 197) + '…' : firstLine;
}
