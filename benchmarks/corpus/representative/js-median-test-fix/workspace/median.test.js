// median.test.js — node:test suite. Run with `node --test`.
// This test is CORRECT; the defect is in median.js. A fix must make this
// pass without weakening or deleting any case.

const test = require('node:test');
const assert = require('node:assert');
const { median } = require('./median.js');

test('odd length returns the middle value', () => {
  assert.strictEqual(median([3, 1, 2]), 2);
});

test('even length averages the two middle values', () => {
  assert.strictEqual(median([1, 2, 3, 4]), 2.5);
});

test('even length on unsorted input', () => {
  assert.strictEqual(median([10, 2, 8, 4]), 6);
});

test('single element', () => {
  assert.strictEqual(median([42]), 42);
});
