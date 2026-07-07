// Unit tests for src/decorations/cache.ts.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { FileFreshnessCache, DEFAULT_TTL_MS } from '../../src/decorations/cache';
import type { FileStateResponse } from '../../src/api/types';

function mkState(path: string): FileStateResponse {
  return {
    path,
    last_read_at: '2026-06-02T10:00:00Z',
    last_read_by: 'claude-code',
    edit_count_24h: 1,
    stale_rereads_24h: 0,
    tools_touched: ['claude-code'],
  };
}

describe('FileFreshnessCache', () => {
  test('peek returns undefined on miss', () => {
    const cache = new FileFreshnessCache(async () => mkState('/a'));
    assert.equal(cache.peek('/missing'), undefined);
  });

  test('get fetches once and populates the cache', async () => {
    let calls = 0;
    const cache = new FileFreshnessCache(async (p) => {
      calls++;
      return mkState(p);
    });
    const a = await cache.get('/a');
    const b = await cache.get('/a');
    assert.equal(a.path, '/a');
    assert.equal(b.path, '/a');
    assert.equal(calls, 1, 'fetcher must run exactly once for cached path');
    assert.equal(cache.size(), 1);
  });

  test('concurrent gets dedupe to a single in-flight fetch', async () => {
    let calls = 0;
    let resolveFetcher: ((s: FileStateResponse) => void) | undefined;
    const cache = new FileFreshnessCache(
      () =>
        new Promise<FileStateResponse>((res) => {
          calls++;
          resolveFetcher = res;
        }),
    );
    const p1 = cache.get('/c');
    const p2 = cache.get('/c');
    assert.equal(calls, 1, 'second concurrent get must share the in-flight');
    resolveFetcher!(mkState('/c'));
    const [a, b] = await Promise.all([p1, p2]);
    assert.equal(a, b, 'concurrent gets resolve to the same value');
  });

  test('expired entries are dropped and refetched', async () => {
    let calls = 0;
    let nowMs = 1_000_000;
    const cache = new FileFreshnessCache(
      async (p) => {
        calls++;
        return mkState(p);
      },
      () => nowMs,
      100,
    );
    await cache.get('/x');
    nowMs += 101; // pass the TTL boundary
    assert.equal(cache.peek('/x'), undefined, 'expired entry must drop from peek');
    await cache.get('/x');
    assert.equal(calls, 2, 'expired entry must trigger a refetch');
  });

  test('invalidate removes a specific entry', async () => {
    const cache = new FileFreshnessCache(async (p) => mkState(p));
    await cache.get('/a');
    await cache.get('/b');
    cache.invalidate('/a');
    assert.equal(cache.peek('/a'), undefined);
    assert.ok(cache.peek('/b'));
  });

  test('clear empties the cache and in-flight map', () => {
    const cache = new FileFreshnessCache(async (p) => mkState(p));
    cache.clear();
    assert.equal(cache.size(), 0);
  });

  test('exports DEFAULT_TTL_MS as 5 minutes', () => {
    assert.equal(DEFAULT_TTL_MS, 5 * 60 * 1000);
  });
});
