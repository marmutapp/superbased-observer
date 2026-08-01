// usageAnchor.ts — shared "how long has this install had real usage"
// signal (localStorage `sb_milestones_anchor`). Originally private to
// MilestonesCard; extracted so StarPrompt can gate its once-only
// lazygit-style nudge on the SAME anchor MilestonesCard already
// writes, instead of inventing a new backend signal
// (docs/plans/growth-virality-product-review-2026-07-30.md §5 Tier 2).
//
// The anchor records `{ at, sessions }` the first time any consumer
// observes session data — whichever of MilestonesCard / StarPrompt
// mounts first creates it; both then read the same value.

const ANCHOR_KEY = "sb_milestones_anchor";
const WEEK_MS = 7 * 24 * 60 * 60 * 1000;

export type UsageAnchor = { at: string; sessions: number; saved_at_anchor?: number };

function lsGet(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function lsSet(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // ignore — anchor-gated features simply never fire without storage
  }
}

// readUsageAnchor is a pure read: returns the stored anchor, or null
// if none has been created yet. Never writes.
export function readUsageAnchor(): UsageAnchor | null {
  const raw = lsGet(ANCHOR_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as UsageAnchor;
  } catch {
    return null;
  }
}

export function writeUsageAnchor(a: UsageAnchor) {
  lsSet(ANCHOR_KEY, JSON.stringify(a));
}

// getOrCreateUsageAnchor returns the stored anchor, creating one
// (recording `sessions` as the count at first sight) if none exists
// yet. Idempotent and safe to call from multiple components.
export function getOrCreateUsageAnchor(sessions: number): UsageAnchor {
  const existing = readUsageAnchor();
  if (existing) return existing;
  const anchor: UsageAnchor = { at: new Date().toISOString(), sessions };
  writeUsageAnchor(anchor);
  return anchor;
}

// hasOneWeekOfUsage is STRICTLY READ-ONLY — it never creates an
// anchor. Callers that may run before MilestonesCard's render-time
// create+retire pass (e.g. StarPrompt, mounted as a fallback) must
// not race that pass by creating the anchor themselves; this returns
// false when no anchor exists yet rather than creating one. True once
// the (already-existing) anchor is at least a week old.
export function hasOneWeekOfUsage(sessions: number | null): boolean {
  if (sessions == null || sessions <= 0) return false;
  const anchor = readUsageAnchor();
  if (!anchor) return false;
  return Date.now() - new Date(anchor.at).getTime() >= WEEK_MS;
}
