// Wordmark — the short brand mark rendered on camera-ready hero stat
// surfaces (Cost hero stats, Suggestions "avoidable spend" hero,
// MilestonesCard, Report.tsx) so a screenshot posted without any
// export pipeline still carries product attribution (the monkeytype
// model — docs/plans/growth-virality-product-review-2026-07-30.md §4.1).
//
// Mirrors internal/statusline/wordmark.go's `Wordmark` constant
// (`▞ superbased`, U+259E QUADRANT UPPER RIGHT AND LOWER LEFT + a
// space + lowercase "superbased") — the operator-vetoed final brand
// literal (2026-07-30), kept in sync BY HAND like the VS Code status
// bar's WORDMARK constant already is. If the Go literal ever changes,
// this one must change with it.
export const WORDMARK = "▞ superbased";

// The bare domain, used where a full wordmark glyph would compete
// with data (e.g. share-text bodies) but a product mention is still
// wanted.
export const WORDMARK_DOMAIN = "superbased.app";
