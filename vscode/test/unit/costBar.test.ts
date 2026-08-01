// Unit tests for the pure helpers in src/status/costBar-internals.ts.
//
// costBar.ts itself imports `vscode`, which isn't resolvable under the
// plain-node test runner (no runtime shim in this repo — see
// binary.test.ts's own header comment), so the renderHeadline text-format
// logic is exercised through its extracted pure helper instead.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { buildHeadlineText, WORDMARK } from '../../src/status/costBar-internals';

describe('buildHeadlineText', () => {
  test('includes the wordmark when enabled', () => {
    const text = buildHeadlineText(3.42, true);
    assert.ok(text.includes('▞ superbased'));
    assert.equal(text, '$(graph) ▞ superbased $3.42');
  });

  test('omits the wordmark when disabled', () => {
    const text = buildHeadlineText(3.42, false);
    assert.ok(!text.includes('▞ superbased'));
    assert.equal(text, '$(graph) $3.42');
  });

  test('WORDMARK matches the literal kept in sync with internal/statusline/wordmark.go', () => {
    assert.equal(WORDMARK, '▞ superbased');
  });
});
