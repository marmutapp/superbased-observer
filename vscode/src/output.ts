import * as vscode from 'vscode';

let channel: vscode.OutputChannel | undefined;

function ensure(): vscode.OutputChannel {
  if (!channel) {
    channel = vscode.window.createOutputChannel('Observer');
  }
  return channel;
}

export const output = {
  appendLine(message: string): void {
    ensure().appendLine(`[${new Date().toISOString()}] ${message}`);
  },
  show(preserveFocus = true): void {
    ensure().show(preserveFocus);
  },
  dispose(): void {
    channel?.dispose();
    channel = undefined;
  },
};
