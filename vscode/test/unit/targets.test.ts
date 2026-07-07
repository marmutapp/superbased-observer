// Unit tests for the instruction-target classifier.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { instructionTargetFor } from '../../src/codelens/targets';

describe('instructionTargetFor', () => {
  test('maps CLAUDE.md → claude', () => {
    assert.equal(instructionTargetFor('/home/x/repo/CLAUDE.md'), 'claude');
    assert.equal(instructionTargetFor('CLAUDE.md'), 'claude');
  });

  test('maps AGENTS.md → agents', () => {
    assert.equal(instructionTargetFor('/home/x/repo/AGENTS.md'), 'agents');
  });

  test('maps .cursorrules → cursor', () => {
    assert.equal(instructionTargetFor('/home/x/repo/.cursorrules'), 'cursor');
  });

  test('handles Windows backslashes', () => {
    assert.equal(instructionTargetFor('C:\\Users\\x\\repo\\CLAUDE.md'), 'claude');
    assert.equal(instructionTargetFor('C:\\Users\\x\\repo\\.cursorrules'), 'cursor');
  });

  test('returns undefined for files outside the supported triple', () => {
    assert.equal(instructionTargetFor('/home/x/repo/README.md'), undefined);
    assert.equal(instructionTargetFor('/home/x/repo/claude.md'), undefined); // case-sensitive
    assert.equal(instructionTargetFor('/home/x/repo/agents.MD'), undefined);
  });

  test('returns undefined on empty / missing input', () => {
    assert.equal(instructionTargetFor(undefined), undefined);
    assert.equal(instructionTargetFor(''), undefined);
  });

  test('strips trailing path separators before matching', () => {
    assert.equal(instructionTargetFor('/home/x/repo/CLAUDE.md/'), 'claude');
  });
});
