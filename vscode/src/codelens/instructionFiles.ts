// CodeLens provider for CLAUDE.md / AGENTS.md / .cursorrules.
//
// Surfaces two actions at the top of each file:
//   - "Refresh from Observer learnings" (observer suggest --apply)
//   - "Preview suggestions"             (observer suggest, default dry-run)

import * as vscode from 'vscode';
import { instructionTargetFor } from './targets';

export const INSTRUCTION_SELECTORS: vscode.DocumentSelector[] = [
  { pattern: '**/CLAUDE.md' },
  { pattern: '**/AGENTS.md' },
  { pattern: '**/.cursorrules' },
];

export class InstructionFilesCodeLensProvider implements vscode.CodeLensProvider {
  provideCodeLenses(document: vscode.TextDocument): vscode.CodeLens[] {
    const target = instructionTargetFor(document.uri.fsPath);
    if (!target) return [];
    const range = new vscode.Range(0, 0, 0, 0);
    return [
      new vscode.CodeLens(range, {
        title: '$(sync) Refresh from Observer learnings',
        command: 'observer.refreshInstructions',
        arguments: [document.uri, target],
        tooltip:
          `Run \`observer suggest --apply --target ${target}\` for this workspace and ` +
          `reload the file.`,
      }),
      new vscode.CodeLens(range, {
        title: '$(eye) Preview suggestions',
        command: 'observer.previewInstructions',
        arguments: [document.uri, target],
        tooltip: `Run \`observer suggest --target ${target}\` and show the dry-run output beside this file.`,
      }),
    ];
  }
}

export function registerInstructionFilesCodeLens(
  ctx: vscode.ExtensionContext,
): void {
  const provider = new InstructionFilesCodeLensProvider();
  for (const selector of INSTRUCTION_SELECTORS) {
    ctx.subscriptions.push(
      vscode.languages.registerCodeLensProvider(selector, provider),
    );
  }
}
