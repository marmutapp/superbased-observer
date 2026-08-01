import type { SessionDetail } from "@/lib/types";

// Shared helpers for the session-detail modules. Extracted VERBATIM from
// SessionDetailPanel.tsx's trailing "helpers" block during the tab split —
// these are the only helpers more than one module needs, so they live in one
// place rather than being duplicated per tab.

// ----- helpers -----------------------------------------------------

// elapsedMillis measures start→end. `end` is ended_at when the session was
// cleanly closed, else last_activity_at (COALESCE'd server-side to the last
// action's timestamp); Date.now() is only the last resort for a session with
// no end AND no recorded activity. This stops a never-closed session from
// reporting start→now (the 583h bug).
export function elapsedMillis(start: string, end?: string): number | null {
  const s = new Date(start).getTime();
  if (!Number.isFinite(s)) return null;
  const e = end ? new Date(end).getTime() : Date.now();
  if (!Number.isFinite(e)) return null;
  return Math.max(0, e - s);
}

// sessionRecentlyActive — the cheapest in-file honest signal for whether a
// bare session is worth offering a read-only "Watch" on: NOT cleanly ended,
// and last activity within 15 minutes (matching /api/live's window). The
// ended_at exclusion matters because the server sets last_activity_at to
// ended_at for closed sessions — without it a just-ended session would offer
// "Watch" on a conversation that can no longer move. Uses last_activity_at
// (server COALESCE of the newest action timestamp), falling back to
// started_at for a brand-new session that hasn't logged an action yet. A
// dead/stale session returns false → no watch affordance (honest-disabled).
// Known residual: a session active only through api_turns/token_usage rows
// (no recent actions) can under-report here; the Sessions page's watch pill
// uses the canonical /api/live signal and passes watch=true, which overrides.
export function sessionRecentlyActive(d: SessionDetail): boolean {
  if (d.ended_at) return false;
  const iso = d.last_activity_at ?? d.started_at;
  if (!iso) return false;
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return false;
  return Date.now() - t <= 15 * 60 * 1000;
}

// elapsedSub is the Elapsed tile's sub-label. Cleanly closed → the end date.
// Never closed but with recent activity (< 10 min ago) → "session in
// progress". Never closed and stale → the last-activity date, so an old
// unfinished session doesn't misleadingly read as still running.
export function elapsedSub(d: SessionDetail): string {
  if (d.ended_at) return fmtDate(d.ended_at);
  const last = d.last_activity_at;
  if (!last) return "session in progress";
  const lastMs = new Date(last).getTime();
  if (Number.isFinite(lastMs) && Date.now() - lastMs < 10 * 60 * 1000) {
    return "session in progress";
  }
  return `last activity ${fmtDate(last)}`;
}

export function fmtDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function truncate(s: string, n: number): string {
  if (!s) return "";
  return s.length <= n ? s : s.slice(0, n - 1) + "…";
}
