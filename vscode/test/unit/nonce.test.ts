// Unit tests for the webview CSP nonce generator.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { NONCE_LENGTH, generateNonce } from '../../src/webview/nonce';

describe('generateNonce', () => {
  test('returns a string of the documented length', () => {
    const nonce = generateNonce();
    assert.equal(typeof nonce, 'string');
    assert.equal(nonce.length, NONCE_LENGTH);
    assert.equal(NONCE_LENGTH, 32); // 24 bytes → 32 base64url chars
  });

  test('uses only base64url-safe characters', () => {
    const nonce = generateNonce();
    assert.match(nonce, /^[A-Za-z0-9_-]+$/);
  });

  test('is non-deterministic across consecutive calls', () => {
    const seen = new Set<string>();
    for (let i = 0; i < 64; i++) {
      seen.add(generateNonce());
    }
    assert.equal(seen.size, 64, 'expected 64 distinct nonces — collision suggests broken randomness');
  });

  test('exhibits enough byte diversity to be plausibly random', () => {
    // 24 random bytes encoded as 32 base64url chars should hit at
    // least 16 distinct characters across the sample — a sanity
    // check that catches obviously degenerate output (e.g., all
    // zeros, repeated single byte).
    const distinct = new Set(generateNonce().split(''));
    assert.ok(distinct.size >= 16, `nonce had only ${distinct.size} distinct chars`);
  });
});
