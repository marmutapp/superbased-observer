// Pure helpers for src/daemon.ts.
//
// Sized to the lockfile contract emitted by internal/diag/lockfile.go
// on the Go side: one observer-<pid>.lock per running daemon, under
// the DB directory (default ~/.observer/), JSON body
// {pid, started_at, db_path, binary_path}. Multiple locks coexist
// when the operator runs multiple daemons against different configs.

import * as path from 'node:path';
import * as os from 'node:os';
import * as fs from 'node:fs/promises';

export interface LockInfo {
  pid: number;
  started_at: string;
  db_path: string;
  binary_path: string;
}

export type DaemonMode = 'detect' | 'managed' | 'auto';

/**
 * Resolve the observer DB directory. Honours OBSERVER_HOME env var
 * for the operator override; defaults to ~/.observer.
 */
export function dbDirDefault(env: NodeJS.ProcessEnv = process.env): string {
  if (env.OBSERVER_HOME && env.OBSERVER_HOME.length > 0) {
    return env.OBSERVER_HOME;
  }
  return path.join(env.HOME ?? env.USERPROFILE ?? os.homedir(), '.observer');
}

/**
 * Parse a single lockfile body. Returns undefined on malformed JSON,
 * missing fields, or non-integer pid. Type-narrowing is strict so
 * downstream code can rely on `LockInfo` shape without re-validating.
 */
export function parseLockInfo(raw: string): LockInfo | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return undefined;
  }
  if (!parsed || typeof parsed !== 'object') return undefined;
  const obj = parsed as Record<string, unknown>;
  const pid = obj.pid;
  if (typeof pid !== 'number' || !Number.isInteger(pid) || pid <= 0) return undefined;
  const started_at = typeof obj.started_at === 'string' ? obj.started_at : '';
  const db_path = typeof obj.db_path === 'string' ? obj.db_path : '';
  const binary_path = typeof obj.binary_path === 'string' ? obj.binary_path : '';
  return { pid, started_at, db_path, binary_path };
}

/**
 * Glob observer-*.lock under dbDir, parse each, drop entries whose
 * PIDs are not currently running. Sorted ascending by PID for
 * deterministic output (matches the Go-side `LiveLocks`).
 */
export async function readLiveLocks(
  dbDir: string,
  isAlive: (pid: number) => boolean = processAlive,
): Promise<LockInfo[]> {
  let entries: string[];
  try {
    entries = await fs.readdir(dbDir);
  } catch {
    return [];
  }
  const live: LockInfo[] = [];
  for (const name of entries) {
    if (!/^observer-\d+\.lock$/.test(name)) continue;
    let raw: string;
    try {
      raw = await fs.readFile(path.join(dbDir, name), 'utf8');
    } catch {
      continue;
    }
    const info = parseLockInfo(raw);
    if (!info) continue;
    if (!isAlive(info.pid)) continue;
    live.push(info);
  }
  live.sort((a, b) => a.pid - b.pid);
  return live;
}

/**
 * processAlive: POSIX uses kill(pid, 0) which throws ESRCH on dead
 * PIDs; Windows just relies on the kill(0) round-trip too — Node's
 * implementation maps it to OpenProcess + a no-op signal.
 *
 * Permission-denied (EPERM) means the process exists but we can't
 * signal it; for our purposes that's still "alive".
 */
export function processAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    return code === 'EPERM';
  }
}

export type ModeDecision =
  | { action: 'attach'; lock: LockInfo }
  | { action: 'spawn' }
  | { action: 'idle'; reason: string };

/**
 * decideMode applies the three-mode contract from
 * docs/vscode-extension-tracker.md M1:
 *
 * - detect: attach if a live lock exists, otherwise idle. Never spawn.
 * - managed: spawn if no live lock; otherwise attach (the safety rail
 *   that prevents two daemons fighting over the same DB).
 * - auto: identical to managed.
 *
 * Centralising the decision here keeps daemon.ts thin and lets the
 * unit suite exercise the rail exhaustively.
 */
export function decideMode(mode: DaemonMode, live: LockInfo[]): ModeDecision {
  if (live.length > 0) {
    return { action: 'attach', lock: live[0] };
  }
  if (mode === 'detect') {
    return { action: 'idle', reason: 'no daemon running and observer.daemon.mode is "detect"' };
  }
  return { action: 'spawn' };
}
