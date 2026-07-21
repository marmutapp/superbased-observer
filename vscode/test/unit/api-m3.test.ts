// Unit tests for the M3 additions to src/api/client.ts:
// Client.sessions / Client.discover / Client.cost.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { Client } from '../../src/api/client';
import type {
  CostResponse,
  DiscoverResponse,
  SessionsResponse,
} from '../../src/api/types';

const sessionsBody: SessionsResponse = {
  days: 0,
  limit: 20,
  page: 1,
  rows: [
    {
      id: '926fefd5-16fc-42f3-be35-775938e5483d',
      tool: 'claude-code',
      project: '/home/marmutapp/superbased-observer',
      started_at: '2026-06-02T06:10:00Z',
      last_seen_at: '2026-06-02T09:00:44Z',
      duration_seconds: 10244,
      total_actions: 406,
      input_tokens: 162,
      output_tokens: 121070,
      cache_read_tokens: 22286472,
      cache_creation_tokens: 460290,
      total_tokens: 22867994,
      cost_usd: 18.77,
      ai_cost_usd: 18.77,
      tool_cost_usd: 0,
      cost_reliability: 'unreliable',
      models: ['claude-opus-4-7'],
    },
  ],
};

const discoverBody: DiscoverResponse = {
  cross_tool_files: [
    {
      file_path: '/home/x/repo/file.go',
      project: '/home/x/repo',
      tools: ['claude-code', 'cowork'],
      accesses: 9,
    },
  ],
};

const costBody: CostResponse = {
  group_by: 'model',
  source: 'auto',
  days: 7,
  since: '2026-05-26T00:00:00Z',
  rows: [
    {
      key: 'claude-opus-4-7',
      tokens: {
        input: 46_223,
        output: 3_525_313,
        cache_read: 1_498_049_369,
        cache_creation: 15_081_469,
        reasoning: 0,
        web_search_requests: 0,
      },
      cost_usd: 988.2,
      ai_cost_usd: 988.2,
      tool_cost_usd: 0,
      turn_count: 4663,
      source: 'jsonl',
      reliability: 'unreliable',
      pricing_source: 'exact',
    },
  ],
};

function fakeFetchOk(body: unknown): typeof fetch {
  return async () =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
}

describe('Client.sessions', () => {
  test('parses a SessionsResponse', async () => {
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetchOk(sessionsBody) });
    const res = await c.sessions(20);
    assert.equal(res.rows.length, 1);
    assert.equal(res.rows[0].tool, 'claude-code');
    assert.equal(res.rows[0].duration_seconds, 10244);
  });

  test('throws on non-2xx', async () => {
    const fakeFetch: typeof fetch = async () =>
      new Response('boom', { status: 500, statusText: 'Internal Server Error' });
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetch });
    await assert.rejects(c.sessions(20), /HTTP 500/);
  });
});

describe('Client.discover', () => {
  test('parses a DiscoverResponse', async () => {
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetchOk(discoverBody) });
    const res = await c.discover();
    assert.equal(res.cross_tool_files[0].tools.length, 2);
    assert.equal(res.cross_tool_files[0].accesses, 9);
  });
});

describe('Client.cost', () => {
  test('parses a CostResponse', async () => {
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetchOk(costBody) });
    const res = await c.cost(7, 'model');
    assert.equal(res.group_by, 'model');
    assert.equal(res.rows[0].key, 'claude-opus-4-7');
    assert.equal(res.rows[0].tokens.cache_read, 1_498_049_369);
  });

  test('encodes the groupBy query argument', async () => {
    let capturedUrl = '';
    const fakeFetch: typeof fetch = async (input) => {
      capturedUrl = String(input);
      return new Response(JSON.stringify(costBody), { status: 200 });
    };
    const c = new Client({ dashboardPort: 8081, fetchImpl: fakeFetch });
    await c.cost(7, 'tool/model');
    assert.match(capturedUrl, /group-by=tool%2Fmodel/);
  });
});
