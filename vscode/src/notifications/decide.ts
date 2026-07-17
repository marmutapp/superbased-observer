// Pure decision helpers for the M5 notifications. Lives in its own
// module so the unit suite can pin the threshold + dedup logic
// without spinning up a vscode runtime.

import type { HeadlineResponse, WatcherHealthResponse } from '../api/types';

export const BUDGET_PCT_THRESHOLD = 80;
export const WATCHER_BEHIND_BYTES_THRESHOLD = 10 * 1024;
export const WATCHER_DEDUP_MS = 5 * 60 * 1000;

export interface BudgetDecision {
  fire: boolean;
  pct?: number;
  toDateUsd?: number;
  budgetUsd?: number;
}

/**
 * decideBudget returns whether to surface the breach notification
 * for this headline payload. Fires only when:
 *
 *  - a positive monthly budget is configured (month.budget_usd > 0),
 *  - month.budget_pct >= 80, AND
 *  - we haven't already fired today (caller checks `lastFiredDay`).
 *
 * The "calendar day" is the UTC YYYY-MM-DD of `now` — keeps the
 * dedup deterministic across timezones and easy to test.
 */
export function decideBudget(
  data: HeadlineResponse,
  lastFiredDay: string | undefined,
  now: Date,
): BudgetDecision {
  const budgetUsd = data.month?.budget_usd ?? 0;
  const pct = data.month?.budget_pct ?? 0;
  if (budgetUsd <= 0 || pct < BUDGET_PCT_THRESHOLD) return { fire: false };
  const today = now.toISOString().slice(0, 10);
  if (lastFiredDay === today) return { fire: false };
  return {
    fire: true,
    pct,
    toDateUsd: data.month?.to_date_usd,
    budgetUsd,
  };
}

export interface WatcherDecision {
  fire: boolean;
  totalBytes?: number;
  worstFile?: string;
  worstBytes?: number;
}

/**
 * decideWatcherLag returns whether to surface the watcher-lag
 * notification for this /api/health/watcher payload.
 *
 *  - Threshold: behind_total_bytes > 10 KB (matches plan §9).
 *  - Dedup: identified by the worst-lagging file path; fire at most
 *    once per WATCHER_DEDUP_MS for the same path.
 *
 * The caller maintains the dedup map (`lastFiredAtMs` keyed by
 * worst-file path) and updates it when `fire === true`. We surface
 * the worst file in the toast so the operator can act on it directly.
 */
export function decideWatcherLag(
  data: WatcherHealthResponse,
  lastFiredAtMs: Map<string, number>,
  now: number,
): WatcherDecision {
  if ((data.behind_total_bytes ?? 0) <= WATCHER_BEHIND_BYTES_THRESHOLD) {
    return { fire: false };
  }
  const worst = (data.files ?? [])
    .filter((f) => !f.suspected_misrouted) // misrouted rows are stuck-by-design; don't nag
    .reduce<{ path: string; behind: number } | undefined>((acc, f) => {
      const behind = f.behind_bytes ?? 0;
      if (!acc || behind > acc.behind) return { path: f.path, behind };
      return acc;
    }, undefined);
  if (!worst) return { fire: false };
  const lastAt = lastFiredAtMs.get(worst.path);
  if (lastAt !== undefined && now - lastAt < WATCHER_DEDUP_MS) {
    return { fire: false };
  }
  return {
    fire: true,
    totalBytes: data.behind_total_bytes,
    worstFile: worst.path,
    worstBytes: worst.behind,
  };
}
