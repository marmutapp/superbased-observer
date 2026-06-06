// Unit tests for src/api/client.ts pure-logic surfaces.
//
// Client.headline / Client.health are exercised against a stubbed
// fetch so the suite runs offline. withBackoff is exercised with a
// fake sleep so the retry loop is instantaneous.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { Client, DEFAULT_BACKOFF_DELAYS, withBackoff } from '../../src/api/client';
import type { HeadlineResponse } from '../../src/api/types';

describe('Client.url', () => {
  test('builds a 127.0.0.1 URL on the configured port', () => {
    const c = new Client({ dashboardPort: 8123 });
    assert.equal(c.url('/api/analysis/headline'), 'http://127.0.0.1:8123/api/analysis/headline');
  });

  test('prepends a slash when the caller omits one', () => {
    const c = new Client({ dashboardPort: 8081 });
    assert.equal(c.url('api/health'), 'http://127.0.0.1:8081/api/health');
  });

  test('respects the host override (Remote-SSH staging)', () => {
    const c = new Client({ dashboardPort: 8081, host: '10.0.0.5' });
    assert.equal(c.url('/api/x'), 'http://10.0.0.5:8081/api/x');
  });
});

describe('Client.headline', () => {
  test('parses a typed HeadlineResponse from a stubbed fetch', async () => {
    const payload: HeadlineResponse = {
      days: 1,
      period: {
        cost_usd: 313.85,
        delta_pct: 20.5,
        period_start: '2026-06-01T07:30:01Z',
        prior_cost_usd: 260.54,
        prior_is_zero: false,
        prior_start: '2026-05-31T07:30:01Z',
        recorded_cost_share_pct: 0,
      },
      top_model: { key: 'claude-opus-4-7', cost_usd: 269.27, concentration_pct: 85.79 },
      month: {
        budget_pct: 0,
        budget_usd: 0,
        days_elapsed: 2,
        days_in_month: 30,
        prior_month_is_zero: false,
        prior_month_same_day_usd: 131.4,
        projection_usd: 5370.46,
        to_date_usd: 358.03,
        vs_prior_month_pct: 172.46,
      },
      burn_rate: { active_hours: 20, cost_per_hour_usd: 15.69 },
    };
    const fakeFetch: typeof fetch = async () =>
      new Response(JSON.stringify(payload), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetch });
    const data = await c.headline(1);
    assert.equal(data.period.cost_usd, 313.85);
    assert.equal(data.top_model.key, 'claude-opus-4-7');
  });

  test('throws on a non-2xx response', async () => {
    const fakeFetch: typeof fetch = async () =>
      new Response('boom', { status: 500, statusText: 'Internal Server Error' });
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetch });
    await assert.rejects(c.headline(1), /HTTP 500/);
  });
});

describe('withBackoff', () => {
  test('returns on the first successful attempt without sleeping', async () => {
    let sleeps = 0;
    const result = await withBackoff(async () => 'ok', {
      sleep: async () => {
        sleeps += 1;
      },
    });
    assert.equal(result, 'ok');
    assert.equal(sleeps, 0);
  });

  test('retries through the configured delays and surfaces success', async () => {
    let calls = 0;
    const slept: number[] = [];
    const result = await withBackoff(
      async () => {
        calls += 1;
        if (calls < 3) throw new Error(`attempt ${calls} failed`);
        return 'recovered';
      },
      {
        delaysMs: [10, 20, 30],
        sleep: async (ms) => {
          slept.push(ms);
        },
      },
    );
    assert.equal(result, 'recovered');
    assert.equal(calls, 3);
    assert.deepEqual(slept, [10, 20]); // two sleeps before the third (winning) attempt
  });

  test('propagates the last error when every attempt fails', async () => {
    let calls = 0;
    await assert.rejects(
      withBackoff(
        async () => {
          calls += 1;
          throw new Error(`attempt ${calls}`);
        },
        { delaysMs: [1, 1], sleep: async () => undefined },
      ),
      /attempt 3/,
    );
    assert.equal(calls, 3);
  });

  test('DEFAULT_BACKOFF_DELAYS is a frozen 5-step sequence', () => {
    assert.deepEqual([...DEFAULT_BACKOFF_DELAYS], [200, 500, 1_000, 2_000, 5_000]);
    assert.ok(Object.isFrozen(DEFAULT_BACKOFF_DELAYS));
  });
});
