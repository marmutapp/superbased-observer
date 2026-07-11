// Unit tests for src/daemon-internals.ts pure helpers.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import * as os from 'node:os';
import * as path from 'node:path';
import * as fs from 'node:fs/promises';
import {
  dbDirDefault,
  decideMode,
  parseLockInfo,
  readLiveLocks,
} from '../../src/daemon-internals';

describe('parseLockInfo', () => {
  test('parses a well-formed lockfile body', () => {
    const raw = JSON.stringify({
      pid: 5670,
      started_at: '2026-06-02T03:49:00Z',
      db_path: '/home/marmutapp/.observer/observer.db',
      binary_path: '/home/marmutapp/superbased-observer/bin/observer',
    });
    const info = parseLockInfo(raw);
    assert.ok(info);
    assert.equal(info?.pid, 5670);
    assert.equal(info?.db_path, '/home/marmutapp/.observer/observer.db');
  });

  test('returns undefined on malformed JSON', () => {
    assert.equal(parseLockInfo('{not json'), undefined);
  });

  test('returns undefined when pid is missing', () => {
    assert.equal(parseLockInfo(JSON.stringify({ db_path: '/x' })), undefined);
  });

  test('returns undefined when pid is non-integer or non-positive', () => {
    assert.equal(parseLockInfo(JSON.stringify({ pid: 0 })), undefined);
    assert.equal(parseLockInfo(JSON.stringify({ pid: -1 })), undefined);
    assert.equal(parseLockInfo(JSON.stringify({ pid: 1.5 })), undefined);
    assert.equal(parseLockInfo(JSON.stringify({ pid: 'string' })), undefined);
  });

  test('tolerates missing optional string fields by emitting empty strings', () => {
    const raw = JSON.stringify({ pid: 1234 });
    const info = parseLockInfo(raw);
    assert.ok(info);
    assert.equal(info?.db_path, '');
    assert.equal(info?.binary_path, '');
    assert.equal(info?.started_at, '');
  });
});

describe('dbDirDefault', () => {
  test('honours OBSERVER_HOME when set', () => {
    assert.equal(
      dbDirDefault({ OBSERVER_HOME: '/custom/observer', HOME: '/home/x' }),
      '/custom/observer',
    );
  });

  test('falls back to ~/.observer when OBSERVER_HOME is unset', () => {
    assert.equal(dbDirDefault({ HOME: '/home/x' }), '/home/x/.observer');
  });

  test('uses USERPROFILE on Windows-style env', () => {
    assert.equal(
      dbDirDefault({ USERPROFILE: 'C:\\Users\\x' } as NodeJS.ProcessEnv),
      path.join('C:\\Users\\x', '.observer'),
    );
  });
});

describe('decideMode', () => {
  const lock = (pid: number) => ({
    pid,
    started_at: '',
    db_path: '',
    binary_path: '',
  });

  test('detect + no live locks → idle', () => {
    const d = decideMode('detect', []);
    assert.equal(d.action, 'idle');
  });

  test('detect + live lock → attach (never spawn)', () => {
    const d = decideMode('detect', [lock(123)]);
    assert.equal(d.action, 'attach');
    if (d.action === 'attach') assert.equal(d.lock.pid, 123);
  });

  test('managed + no live locks → spawn', () => {
    assert.equal(decideMode('managed', []).action, 'spawn');
  });

  test('managed + live lock → attach (safety rail)', () => {
    const d = decideMode('managed', [lock(456)]);
    assert.equal(d.action, 'attach');
    if (d.action === 'attach') assert.equal(d.lock.pid, 456);
  });

  test('auto + no live locks → spawn', () => {
    assert.equal(decideMode('auto', []).action, 'spawn');
  });

  test('auto + live lock → attach', () => {
    assert.equal(decideMode('auto', [lock(789)]).action, 'attach');
  });

  test('picks the first (lowest-PID) lock when multiple are live', () => {
    const d = decideMode('auto', [lock(100), lock(200), lock(50)]);
    assert.equal(d.action, 'attach');
    if (d.action === 'attach') assert.equal(d.lock.pid, 100);
  });
});

describe('readLiveLocks', () => {
  test('returns [] when dbDir does not exist', async () => {
    const ghost = path.join(os.tmpdir(), `observer-no-such-${Date.now()}`);
    assert.deepEqual(await readLiveLocks(ghost), []);
  });

  test('skips files that do not match observer-<digits>.lock', async () => {
    const tmp = await fs.mkdtemp(path.join(os.tmpdir(), 'observer-locks-'));
    try {
      await fs.writeFile(path.join(tmp, 'README.md'), 'unrelated');
      await fs.writeFile(path.join(tmp, 'observer-foo.lock'), '{}');
      await fs.writeFile(path.join(tmp, 'observer-.lock'), '{}');
      const live = await readLiveLocks(tmp, () => true);
      assert.deepEqual(live, []);
    } finally {
      await fs.rm(tmp, { recursive: true, force: true });
    }
  });

  test('returns parsed locks sorted by PID, filtered by isAlive', async () => {
    const tmp = await fs.mkdtemp(path.join(os.tmpdir(), 'observer-locks-'));
    try {
      await fs.writeFile(
        path.join(tmp, 'observer-200.lock'),
        JSON.stringify({ pid: 200, started_at: '', db_path: '/d', binary_path: '/b' }),
      );
      await fs.writeFile(
        path.join(tmp, 'observer-100.lock'),
        JSON.stringify({ pid: 100, started_at: '', db_path: '/d', binary_path: '/b' }),
      );
      await fs.writeFile(
        path.join(tmp, 'observer-300.lock'),
        JSON.stringify({ pid: 300, started_at: '', db_path: '/d', binary_path: '/b' }),
      );
      // Mark 200 as dead so the filter actually fires.
      const live = await readLiveLocks(tmp, (pid) => pid !== 200);
      assert.deepEqual(
        live.map((l) => l.pid),
        [100, 300],
      );
    } finally {
      await fs.rm(tmp, { recursive: true, force: true });
    }
  });

  test('silently skips malformed lock bodies', async () => {
    const tmp = await fs.mkdtemp(path.join(os.tmpdir(), 'observer-locks-'));
    try {
      await fs.writeFile(path.join(tmp, 'observer-7.lock'), 'not-json');
      await fs.writeFile(
        path.join(tmp, 'observer-8.lock'),
        JSON.stringify({ pid: 8 }),
      );
      const live = await readLiveLocks(tmp, () => true);
      assert.deepEqual(
        live.map((l) => l.pid),
        [8],
      );
    } finally {
      await fs.rm(tmp, { recursive: true, force: true });
    }
  });
});
