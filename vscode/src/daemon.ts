// Daemon lifecycle manager.
//
// Wraps the existing `observer start` binary: never re-implements
// its logic. Mode gating + lockfile safety rails come from
// daemon-internals.ts. This module is the "lifecycle owner" that
// holds the ChildProcess handle and the active LockInfo (when
// attached or managed) plus the vscode notification surface.

import * as vscode from 'vscode';
import { spawn, ChildProcess } from 'node:child_process';
import type { ResolvedBinary } from './binary';
import { Client, withBackoff } from './api/client';
import { planNextRestart } from './crashRecovery';
import {
  DaemonMode,
  LockInfo,
  ModeDecision,
  dbDirDefault,
  decideMode,
  processAlive,
  readLiveLocks,
} from './daemon-internals';
import { output } from './output';

export interface DaemonState {
  readonly mode: DaemonMode;
  readonly status: 'idle' | 'attached' | 'managed';
  readonly lock?: LockInfo;
  readonly dashboardPort: number;
  readonly proxyPort: number;
}

export interface DaemonManagerOptions {
  binary: ResolvedBinary;
  dashboardPort: number;
  proxyPort: number;
  mode: DaemonMode;
}

const SIGTERM_GRACE_MS = 5_000;

export class DaemonManager implements vscode.Disposable {
  private state: DaemonState;
  private child: ChildProcess | undefined;
  private readonly client: Client;
  private readonly binary: ResolvedBinary;
  private readonly _onDidChangeState = new vscode.EventEmitter<DaemonState>();
  readonly onDidChangeState = this._onDidChangeState.event;
  /**
   * Set on the entry to stop(); cleared on a successful start. Tells
   * the exit handler whether the child died by our hand (don't
   * recover) or unexpectedly (recover via backoff).
   */
  private isStopping = false;
  /** Restart attempt count since the last successful health probe. */
  private restartAttempt = 0;
  private recoveryTimer: NodeJS.Timeout | undefined;

  constructor(opts: DaemonManagerOptions) {
    this.binary = opts.binary;
    this.state = {
      mode: opts.mode,
      status: 'idle',
      dashboardPort: opts.dashboardPort,
      proxyPort: opts.proxyPort,
    };
    this.client = new Client({ dashboardPort: opts.dashboardPort });
  }

  getState(): DaemonState {
    return this.state;
  }

  getClient(): Client {
    return this.client;
  }

  /**
   * Apply the activation-time decision: attach if a live lock is
   * found, spawn if mode allows, idle otherwise. Safe to call
   * repeatedly — re-evaluates fresh state each time.
   */
  async reconcile(): Promise<DaemonState> {
    const live = await readLiveLocks(dbDirDefault());
    const decision = decideMode(this.state.mode, live);
    await this.apply(decision);
    return this.state;
  }

  private async apply(decision: ModeDecision): Promise<void> {
    switch (decision.action) {
      case 'attach':
        output.appendLine(
          `Attaching to existing daemon PID ${decision.lock.pid} (db_path=${decision.lock.db_path})`,
        );
        this.setState({ status: 'attached', lock: decision.lock });
        return;
      case 'spawn':
        await this.spawnDaemon();
        return;
      case 'idle':
        output.appendLine(`Daemon idle: ${decision.reason}`);
        this.setState({ status: 'idle', lock: undefined });
        return;
    }
  }

  /**
   * Explicit start. Used by the observer.start command. Honours the
   * same safety rail as reconcile().
   */
  async start(): Promise<DaemonState> {
    if (this.state.mode === 'detect') {
      void vscode.window.showInformationMessage(
        'Observer: daemon.mode is "detect" — set it to "managed" or "auto" to allow the extension to spawn the daemon.',
      );
      return this.state;
    }
    const live = await readLiveLocks(dbDirDefault());
    if (live.length > 0) {
      void vscode.window.showInformationMessage(
        `Observer: already running (PID ${live[0].pid}) — attaching instead.`,
      );
      this.setState({ status: 'attached', lock: live[0] });
      return this.state;
    }
    await this.spawnDaemon();
    return this.state;
  }

  /**
   * Explicit stop. Only kills daemons WE spawned (status === managed).
   * Attached daemons are owned by whoever started them — we never
   * touch their process.
   */
  async stop(): Promise<DaemonState> {
    if (this.state.status !== 'managed' || !this.child) {
      void vscode.window.showInformationMessage(
        'Observer: no extension-managed daemon to stop.',
      );
      return this.state;
    }
    this.isStopping = true;
    this.cancelPendingRecovery();
    const pid = this.child.pid;
    output.appendLine(`Stopping managed daemon PID ${pid}`);
    try {
      this.child.kill('SIGTERM');
    } catch (err) {
      output.appendLine(`SIGTERM failed: ${(err as Error).message}`);
    }
    await new Promise((r) => setTimeout(r, SIGTERM_GRACE_MS));
    if (pid && processAlive(pid)) {
      output.appendLine(`PID ${pid} still alive after ${SIGTERM_GRACE_MS}ms — sending SIGKILL`);
      try {
        this.child.kill('SIGKILL');
      } catch (err) {
        output.appendLine(`SIGKILL failed: ${(err as Error).message}`);
      }
    }
    this.child = undefined;
    this.setState({ status: 'idle', lock: undefined });
    this.isStopping = false;
    return this.state;
  }

  private async spawnDaemon(): Promise<void> {
    const args = [
      'start',
      '--dashboard-addr',
      `127.0.0.1:${this.state.dashboardPort}`,
      '--port',
      String(this.state.proxyPort),
    ];
    output.appendLine(`Spawning ${this.binary.path} ${args.join(' ')}`);
    const child = spawn(this.binary.path, args, {
      detached: false,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    child.stdout?.on('data', (buf: Buffer) => {
      output.appendLine(`[daemon] ${buf.toString('utf8').trimEnd()}`);
    });
    child.stderr?.on('data', (buf: Buffer) => {
      output.appendLine(`[daemon!] ${buf.toString('utf8').trimEnd()}`);
    });
    child.once('exit', (code, signal) => {
      output.appendLine(`Daemon exited (code=${code} signal=${signal})`);
      if (this.child !== child) return;
      this.child = undefined;
      this.setState({ status: 'idle', lock: undefined });
      if (!this.isStopping && this.canAutoSpawn()) {
        this.scheduleCrashRecovery();
      }
    });
    this.child = child;
    this.setState({
      status: 'managed',
      lock: child.pid
        ? {
            pid: child.pid,
            started_at: new Date().toISOString(),
            db_path: '',
            binary_path: this.binary.path,
          }
        : undefined,
    });

    // Wait for the dashboard to come up before returning. The Go side
    // emits a "dashboard listening at …" line on stderr; we wait on
    // an HTTP probe instead — narrower contract.
    try {
      await withBackoff(() => this.client.health());
      output.appendLine(`Daemon dashboard ready at ${this.client.url('/')}`);
      this.restartAttempt = 0;
    } catch (err) {
      output.appendLine(`Daemon health probe failed: ${(err as Error).message}`);
      void vscode.window.showErrorMessage(
        `Observer: daemon started but dashboard did not respond on :${this.state.dashboardPort}. Check the Output channel.`,
      );
    }
  }

  private canAutoSpawn(): boolean {
    return this.state.mode === 'managed' || this.state.mode === 'auto';
  }

  private scheduleCrashRecovery(): void {
    const action = planNextRestart(this.restartAttempt);
    if (action.kind === 'escalate') {
      this.cancelPendingRecovery();
      this.restartAttempt = 0;
      void this.escalateCrash();
      return;
    }
    output.appendLine(
      `Scheduling crash recovery attempt ${action.attempt}/3 in ${action.delayMs}ms`,
    );
    this.cancelPendingRecovery();
    this.recoveryTimer = setTimeout(() => {
      this.recoveryTimer = undefined;
      this.restartAttempt = action.attempt;
      void this.attemptCrashRestart();
    }, action.delayMs);
  }

  private async attemptCrashRestart(): Promise<void> {
    if (this.isStopping || !this.canAutoSpawn()) return;
    output.appendLine(`Crash-recovery restart attempt ${this.restartAttempt}/3`);
    try {
      await this.spawnDaemon();
    } catch (err) {
      output.appendLine(`Restart attempt failed synchronously: ${(err as Error).message}`);
      this.scheduleCrashRecovery();
    }
  }

  private async escalateCrash(): Promise<void> {
    const message =
      'Observer: daemon crashed 4 times in a row — auto-restart paused. Check the Output channel.';
    const choice = await vscode.window.showErrorMessage(
      message,
      'Open Output Channel',
      'Retry',
    );
    if (choice === 'Open Output Channel') {
      void vscode.commands.executeCommand('observer.showOutput');
    } else if (choice === 'Retry') {
      void this.attemptCrashRestart();
    }
  }

  private cancelPendingRecovery(): void {
    if (this.recoveryTimer) {
      clearTimeout(this.recoveryTimer);
      this.recoveryTimer = undefined;
    }
  }

  private setState(patch: Partial<DaemonState>): void {
    this.state = { ...this.state, ...patch };
    this._onDidChangeState.fire(this.state);
  }

  dispose(): void {
    this.cancelPendingRecovery();
    if (this.state.status === 'managed' && this.child) {
      this.isStopping = true;
      try {
        this.child.kill('SIGTERM');
      } catch {
        /* best effort */
      }
    }
    this._onDidChangeState.dispose();
  }
}
