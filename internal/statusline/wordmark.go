package statusline

// Wordmark is the short brand mark printed first on every `observer
// statusline` render, in every fail-open state including the maximally
// degraded one (no daemon, no stdin JSON) — presence in the screenshots
// people already take is the entire point of this feature (plan §4.1:
// "the wordmark... always first, always present").
//
// This literal is an operator-vetoable BRAND decision, not an engineering
// one (plan §3.1/§3.2). The plan narrowed a short list of candidates
// (`▞ superbased`, `sb·`, a sigil+letters variant) without picking a
// final one unilaterally; `sb·` was the WP0 status block's PROVISIONAL
// decision lock (2026-07-30), explicitly flagged there as still subject
// to an operator veto (plan §9 item 1, WP9's AC). The operator exercised
// that veto on 2026-07-30 and locked `▞ superbased` (U+259E QUADRANT
// UPPER RIGHT AND LOWER LEFT, a space, then lowercase "superbased") as
// the final wordmark, superseding the provisional `sb·`.
//
// IMPORTANT — kept in sync BY HAND, not by any shared build step: the VS
// Code status bar's success-path text (vscode/src/status/costBar-internals.ts,
// the WORDMARK constant, consumed by costBar.ts's renderHeadline) carries
// the identical literal so the terminal and the editor show the same
// mark (plan §1.3, §3.3). Go and TypeScript have no shared-constant
// mechanism in this codebase for a value this small — two hand-kept
// literals with a cross-referencing comment on each side is the plan's
// deliberately low-machinery answer (plan §3.3), not an oversight. If
// Wordmark ever changes, costBar-internals.ts's WORDMARK literal must
// change with it.
const Wordmark = "▞ superbased"
