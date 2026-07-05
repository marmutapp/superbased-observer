// Unit tests for src/notifications/decide.ts.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import {
  BUDGET_PCT_THRESHOLD,
  WATCHER_BEHIND_BYTES_THRESHOLD,
  WATCHER_DEDUP_MS,
  decideBudget,
  decideWatcherLag,
} from '../../src/notifications/decide';
import type { HeadlineResponse, WatcherHealthResponse } from '../../src/api/types';

function headline(budgetUsd: number, pct: number): HeadlineResponse {
  return {
    days: 1,
    period: {
      cost_usd: 0,
      delta_pct: 0,
      period_start: '',
      prior_cost_usd: 0,
      prior_is_zero: false,
      prior_start: '',
      recorded_cost_share_pct: 0,
    },
    top_model: { key: 'm', cost_usd: 0, concentration_pct: 0 },
    month: {
      budget_pct: pct,
      budget_usd: budgetUsd,
      days_elapsed: 1,
      days_in_month: 30,
      prior_month_is_zero: false,
      prior_month_same_day_usd: 0,
      projection_usd: 0,
      to_date_usd: (pct / 100) * budgetUsd,
      vs_prior_month_pct: 0,
    },
    burn_rate: { active_hours: 0, cost_per_hour_usd: 0 },
  };
}

describe('decideBudget', () => {
  test('does not fire when budget_usd is zero', () => {
    const d = decideBudget(headline(0, 99), undefined, new Date('2026-06-02T00:00:00Z'));
    assert.equal(d.fire, false);
  });
  test('does not fire below threshold', () => {
    const d = decideBudget(headline(100, 79.9), undefined, new Date('2026-06-02T00:00:00Z'));
    assert.equal(d.fire, false);
  });
  test('fires at the threshold boundary (80%)', () => {
    const d = decideBudget(headline(100, 80), undefined, new Date('2026-06-02T00:00:00Z'));
    assert.equal(d.fire, true);
    assert.equal(d.pct, 80);
    assert.equal(d.budgetUsd, 100);
  });
  test('deduped to once per UTC calendar day', () => {
    const d = decideBudget(headline(100, 95), '2026-06-02', new Date('2026-06-02T22:00:00Z'));
    assert.equal(d.fire, false);
  });
  test('fires again the next day', () => {
    const d = decideBudget(headline(100, 95), '2026-06-02', new Date('2026-06-03T01:00:00Z'));
    assert.equal(d.fire, true);
  });
  test('exports threshold constant', () => {
    assert.equal(BUDGET_PCT_THRESHOLD, 80);
  });
});

function watcher(
  totalBytes: number,
  files: Array<{ path: string; behind_bytes: number; suspected_misrouted?: boolean }>,
): WatcherHealthResponse {
  return {
    behind_count: files.length,
    behind_total_bytes: totalBytes,
    checked_at: '2026-06-02T10:00:00Z',
    files: files.map((f) => ({
      path: f.path,
      byte_offset: 0,
      file_size: 0,
      behind_bytes: f.behind_bytes,
      suspected_misrouted: f.suspected_misrouted,
    })),
  };
}

describe('decideWatcherLag', () => {
  test('does not fire under 10 kB total', () => {
    const d = decideWatcherLag(
      watcher(8 * 1024, [{ path: '/x', behind_bytes: 8 * 1024 }]),
      new Map(),
      0,
    );
    assert.equal(d.fire, false);
  });
  test('fires above 10 kB and picks the worst file', () => {
    const d = decideWatcherLag(
      watcher(50_000, [
        { path: '/small', behind_bytes: 5_000 },
        { path: '/worst', behind_bytes: 40_000 },
        { path: '/medium', behind_bytes: 5_000 },
      ]),
      new Map(),
      0,
    );
    assert.equal(d.fire, true);
    assert.equal(d.worstFile, '/worst');
    assert.equal(d.worstBytes, 40_000);
    assert.equal(d.totalBytes, 50_000);
  });
  test('skips suspected_misrouted files when picking the worst', () => {
    const d = decideWatcherLag(
      watcher(50_000, [
        { path: '/misrouted', behind_bytes: 40_000, suspected_misrouted: true },
        { path: '/real', behind_bytes: 12_000 },
      ]),
      new Map(),
      0,
    );
    assert.equal(d.fire, true);
    assert.equal(d.worstFile, '/real');
  });
  test('deduped per worst-file path on a 5-min window', () => {
    const m = new Map<string, number>([['/worst', 100_000]]);
    const within = decideWatcherLag(
      watcher(50_000, [{ path: '/worst', behind_bytes: 40_000 }]),
      m,
      100_000 + 30_000,
    );
    assert.equal(within.fire, false);
    const after = decideWatcherLag(
      watcher(50_000, [{ path: '/worst', behind_bytes: 40_000 }]),
      m,
      100_000 + WATCHER_DEDUP_MS + 1,
    );
    assert.equal(after.fire, true);
  });
  test('exports threshold constants', () => {
    assert.equal(WATCHER_BEHIND_BYTES_THRESHOLD, 10 * 1024);
    assert.equal(WATCHER_DEDUP_MS, 5 * 60 * 1000);
  });
});
