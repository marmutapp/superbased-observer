// Pure helpers for the today-spend status bar item (src/status/costBar.ts).
//
// Kept vscode-import-free so these can be unit-tested without a runtime
// shim (mirrors src/binary-internals.ts / src/daemon-internals.ts — this
// codebase's convention for testing logic that would otherwise live behind
// a `vscode` import).

import { formatUSD } from '../views/format';

// IMPORTANT — kept in sync BY HAND, not by any shared build step: this is
// the identical literal declared in internal/statusline/wordmark.go's
// `Wordmark` constant (see that file's own cross-reference comment back to
// here). Go and TypeScript have no shared-constant mechanism in this
// codebase for a value this small (observer-statusline-plan-2026-07-30.md
// §3.3); if Wordmark ever changes there, this literal must change here too.
// The operator locked `▞ superbased` (U+259E QUADRANT UPPER RIGHT AND
// LOWER LEFT + a space + "superbased") on 2026-07-30, superseding the
// provisional `sb·`.
export const WORDMARK = '▞ superbased';

/**
 * Build the success-path status bar text for the today-spend item.
 *
 * `wordmarkEnabled` gates the `observer.statusBar.wordmark` prefix (default
 * true); it never affects `renderIdle`/`renderDegraded`, which already
 * carry the "SuperBased" brand name unconditionally.
 */
export function buildHeadlineText(spend: number, wordmarkEnabled: boolean): string {
  const prefix = wordmarkEnabled ? `${WORDMARK} ` : '';
  return `$(graph) ${prefix}${formatUSD(spend)}`;
}
