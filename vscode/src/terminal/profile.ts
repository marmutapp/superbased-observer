// Contributed terminal profile that pre-exports the observer
// proxy env vars. Pairs with package.json's
// contributes.terminal.profiles entry under the same id.

import * as vscode from 'vscode';
import type { DaemonManager } from '../daemon';
import { proxyEnvFor } from './envFormat';

const PROFILE_ID = 'observer.terminalProfile';

export function registerTerminalProfile(
  ctx: vscode.ExtensionContext,
  daemon: DaemonManager,
): void {
  const provider: vscode.TerminalProfileProvider = {
    provideTerminalProfile() {
      const env = proxyEnvFor(daemon.getState().proxyPort);
      return new vscode.TerminalProfile({
        name: 'AI Coding Tool (Observer-proxied)',
        env: {
          ANTHROPIC_BASE_URL: env.ANTHROPIC_BASE_URL,
          OPENAI_BASE_URL: env.OPENAI_BASE_URL,
          ENABLE_TOOL_SEARCH: env.ENABLE_TOOL_SEARCH,
        },
      });
    },
  };
  ctx.subscriptions.push(
    vscode.window.registerTerminalProfileProvider(PROFILE_ID, provider),
  );
}
