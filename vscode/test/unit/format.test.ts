// Unit tests for src/views/format.ts pure helpers.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import {
  basename,
  formatDuration,
  formatPct,
  formatTokens,
  formatUSD,
} from '../../src/views/format';

describe('formatUSD', () => {
  test('uses 2-decimal precision under $1000', () => {
    assert.equal(formatUSD(0), '$0.00');
    assert.equal(formatUSD(12.345), '$12.35');
    assert.equal(formatUSD(999.99), '$999.99');
  });

  test('uses thousands separator and 0-decimal at >=$1000', () => {
    assert.equal(formatUSD(1000), '$1,000');
    assert.equal(formatUSD(12345.67), '$12,346');
  });

  test('returns "$—" for non-finite input', () => {
    assert.equal(formatUSD(Number.NaN), '$—');
    assert.equal(formatUSD(Number.POSITIVE_INFINITY), '$—');
  });
});

describe('formatPct', () => {
  test('positive adds a leading +', () => {
    assert.equal(formatPct(12.34), '+12.3%');
  });
  test('negative carries the minus', () => {
    assert.equal(formatPct(-5), '-5.0%');
  });
  test('non-finite → em dash', () => {
    assert.equal(formatPct(Number.NaN), '—');
  });
});

describe('formatTokens', () => {
  test('returns 0 for zero / non-finite input', () => {
    assert.equal(formatTokens(0), '0');
    assert.equal(formatTokens(Number.NaN), '0');
  });
  test('uses k threshold at 1k', () => {
    assert.equal(formatTokens(1_500), '1.5k');
    assert.equal(formatTokens(999), '999');
  });
  test('uses M threshold at 1M', () => {
    assert.equal(formatTokens(2_500_000), '2.50M');
  });
  test('uses B threshold at 1B', () => {
    assert.equal(formatTokens(3_400_000_000), '3.40B');
  });
});

describe('formatDuration', () => {
  test('seconds under 60', () => {
    assert.equal(formatDuration(0), '0s');
    assert.equal(formatDuration(45), '45s');
  });
  test('minutes under 60', () => {
    assert.equal(formatDuration(60), '1m');
    assert.equal(formatDuration(120), '2m');
    assert.equal(formatDuration(3599), '59m');
  });
  test('hours with remaining minutes', () => {
    assert.equal(formatDuration(3600), '1h');
    assert.equal(formatDuration(3660), '1h1m');
    assert.equal(formatDuration(10244), '2h50m'); // operator's claude-code session
  });
  test('non-finite / negative → em dash', () => {
    assert.equal(formatDuration(Number.NaN), '—');
    assert.equal(formatDuration(-1), '—');
  });
});

describe('basename', () => {
  test('strips trailing slashes', () => {
    assert.equal(basename('/home/x/proj/'), 'proj');
  });
  test('handles forward slashes', () => {
    assert.equal(basename('/home/x/file.go'), 'file.go');
  });
  test('handles Windows backslashes', () => {
    assert.equal(basename('C:\\Users\\x\\file.go'), 'file.go');
  });
  test('returns input when no separator present', () => {
    assert.equal(basename('singleword'), 'singleword');
    assert.equal(basename(''), '');
  });
});
