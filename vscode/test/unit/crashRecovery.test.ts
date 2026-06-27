// Unit tests for src/crashRecovery.ts.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { CRASH_RECOVERY_DELAYS_MS, planNextRestart } from '../../src/crashRecovery';

describe('planNextRestart', () => {
  test('schedules the first attempt at 1 s', () => {
    const a = planNextRestart(0);
    assert.equal(a.kind, 'restart');
    if (a.kind === 'restart') {
      assert.equal(a.attempt, 1);
      assert.equal(a.delayMs, 1_000);
    }
  });
  test('schedules the second attempt at 2 s', () => {
    const a = planNextRestart(1);
    assert.equal(a.kind, 'restart');
    if (a.kind === 'restart') {
      assert.equal(a.attempt, 2);
      assert.equal(a.delayMs, 2_000);
    }
  });
  test('schedules the third attempt at 5 s', () => {
    const a = planNextRestart(2);
    assert.equal(a.kind, 'restart');
    if (a.kind === 'restart') {
      assert.equal(a.attempt, 3);
      assert.equal(a.delayMs, 5_000);
    }
  });
  test('escalates after the 3 documented attempts (the 4th failure)', () => {
    assert.equal(planNextRestart(3).kind, 'escalate');
    assert.equal(planNextRestart(99).kind, 'escalate');
  });
  test('escalates on a negative attempt count (defensive)', () => {
    assert.equal(planNextRestart(-1).kind, 'escalate');
  });
  test('exports the documented [1s, 2s, 5s] schedule, frozen', () => {
    assert.deepEqual([...CRASH_RECOVERY_DELAYS_MS], [1_000, 2_000, 5_000]);
    assert.ok(Object.isFrozen(CRASH_RECOVERY_DELAYS_MS));
  });
});
