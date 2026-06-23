// Pure helpers for the DaemonManager's crash-recovery sequencer.
//
// Kept separate so the unit suite can pin the backoff schedule + the
// escalation point without spinning up a vscode runtime.

export const CRASH_RECOVERY_DELAYS_MS = Object.freeze([1_000, 2_000, 5_000]);

export type CrashAction =
  | { kind: 'restart'; attempt: number; delayMs: number }
  | { kind: 'escalate' };

/**
 * planNextRestart returns the action to take after `attempt`
 * consecutive failed spawns. attempt 0 = the original spawn died,
 * attempt 1+ = a restart attempt died.
 *
 * The schedule is fixed at CRASH_RECOVERY_DELAYS_MS — 3 restart
 * attempts at 1 s / 2 s / 5 s. After the 4th failure (attempt = 4
 * after the original + 3 retries) we escalate.
 */
export function planNextRestart(attempt: number): CrashAction {
  if (attempt < 0) return { kind: 'escalate' };
  if (attempt >= CRASH_RECOVERY_DELAYS_MS.length) {
    return { kind: 'escalate' };
  }
  return {
    kind: 'restart',
    attempt: attempt + 1,
    delayMs: CRASH_RECOVERY_DELAYS_MS[attempt],
  };
}
