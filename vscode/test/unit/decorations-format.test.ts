// Unit tests for src/decorations/format.ts.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import {
  decorationBadge,
  decorationTooltip,
  hoverMarkdown,
} from '../../src/decorations/format';
import type { FileStateResponse } from '../../src/api/types';

const empty: FileStateResponse = {
  path: '/p',
  last_read_at: '',
  last_read_by: '',
  edit_count_24h: 0,
  stale_rereads_24h: 0,
  tools_touched: [],
};

describe('decorationBadge', () => {
  test('returns undefined when nothing in the window', () => {
    assert.equal(decorationBadge(empty), undefined);
  });
  test('returns the bullet when read was observed', () => {
    assert.equal(
      decorationBadge({ ...empty, last_read_at: '2026-06-02T10:00Z' }),
      '•',
    );
  });
  test('returns the bullet when edits were observed', () => {
    assert.equal(decorationBadge({ ...empty, edit_count_24h: 2 }), '•');
  });
  test('returns the bullet when stale re-reads were flagged', () => {
    assert.equal(decorationBadge({ ...empty, stale_rereads_24h: 1 }), '•');
  });
});

describe('decorationTooltip', () => {
  test('returns the no-activity fallback when empty', () => {
    assert.equal(decorationTooltip(empty), 'no recent activity');
  });
  test('combines fields with " · " separators', () => {
    const tip = decorationTooltip({
      ...empty,
      last_read_by: 'claude-code',
      edit_count_24h: 12,
      stale_rereads_24h: 3,
      tools_touched: ['claude-code', 'cursor'],
    });
    assert.match(tip, /last read by claude-code/);
    assert.match(tip, /12 edits \/ 24h/);
    assert.match(tip, /3 stale re-reads flagged/);
    assert.match(tip, /tools: claude-code, cursor/);
    assert.match(tip, / · /);
  });
});

describe('hoverMarkdown', () => {
  test('says no recent activity on empty input', () => {
    assert.match(hoverMarkdown(empty), /No recent activity/);
  });
  test('uses the "flagged" framing for stale re-reads (not "avoided")', () => {
    const md = hoverMarkdown({ ...empty, stale_rereads_24h: 3 });
    assert.match(md, /flagged/);
    assert.doesNotMatch(md, /avoided/);
  });
  test('renders last-read + edits + tools when populated', () => {
    const md = hoverMarkdown({
      ...empty,
      last_read_at: '2026-06-02T09:14:22Z',
      last_read_by: 'claude-code',
      edit_count_24h: 5,
      tools_touched: ['claude-code', 'cursor'],
    });
    assert.match(md, /\*\*Observer\*\*/);
    assert.match(md, /Last read by `claude-code`/);
    assert.match(md, /\*\*Edits \(24h\)\*\*: 5/);
    assert.match(md, /Tools touched.*claude-code, cursor/);
  });
});
