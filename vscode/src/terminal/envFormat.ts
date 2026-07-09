// Pure shell-detection + env-var formatter for proxy injection.
//
// Lives in its own module so the unit suite can pin the classifier
// against the same fixtures the live code uses, without spinning up
// a vscode runtime.

export type ShellKind = 'bash' | 'powershell' | 'cmd';

/**
 * detectShell classifies a vscode.env.shell path into one of the
 * three formats copyProxyEnv supports. Unknown / empty input falls
 * back to bash — POSIX defaults are the safer choice when we can't
 * tell, and the worst case is the user just adjusts the syntax.
 */
export function detectShell(shellPath: string | undefined): ShellKind {
  if (!shellPath) return 'bash';
  const lower = shellPath.toLowerCase();
  const base = lower.replace(/\\/g, '/').split('/').pop() ?? lower;
  const stem = base.replace(/\.exe$/, '');
  if (stem === 'powershell' || stem === 'pwsh') return 'powershell';
  if (stem === 'cmd') return 'cmd';
  return 'bash';
}

export interface ProxyEnv {
  ANTHROPIC_BASE_URL: string;
  OPENAI_BASE_URL: string;
  ENABLE_TOOL_SEARCH: string;
}

/**
 * proxyEnvFor builds the env-var triple keyed to the configured
 * proxy port. The OPENAI_BASE_URL includes the /v1 suffix the OpenAI
 * SDK expects.
 */
export function proxyEnvFor(proxyPort: number): ProxyEnv {
  return {
    ANTHROPIC_BASE_URL: `http://127.0.0.1:${proxyPort}`,
    OPENAI_BASE_URL: `http://127.0.0.1:${proxyPort}/v1`,
    ENABLE_TOOL_SEARCH: 'true',
  };
}

/**
 * formatEnv renders the env triple as a single line of shell syntax
 * the user can paste into their terminal. Joined with the shell's
 * conventional statement separator (&& for bash + cmd, ; for
 * PowerShell) so a single paste sets all three.
 */
export function formatEnv(vars: ProxyEnv, shell: ShellKind): string {
  switch (shell) {
    case 'powershell':
      return [
        `$env:ANTHROPIC_BASE_URL='${vars.ANTHROPIC_BASE_URL}'`,
        `$env:OPENAI_BASE_URL='${vars.OPENAI_BASE_URL}'`,
        `$env:ENABLE_TOOL_SEARCH='${vars.ENABLE_TOOL_SEARCH}'`,
      ].join('; ');
    case 'cmd':
      return [
        `set ANTHROPIC_BASE_URL=${vars.ANTHROPIC_BASE_URL}`,
        `set OPENAI_BASE_URL=${vars.OPENAI_BASE_URL}`,
        `set ENABLE_TOOL_SEARCH=${vars.ENABLE_TOOL_SEARCH}`,
      ].join(' && ');
    case 'bash':
      return [
        `export ANTHROPIC_BASE_URL=${vars.ANTHROPIC_BASE_URL}`,
        `export OPENAI_BASE_URL=${vars.OPENAI_BASE_URL}`,
        `export ENABLE_TOOL_SEARCH=${vars.ENABLE_TOOL_SEARCH}`,
      ].join(' && ');
  }
}
