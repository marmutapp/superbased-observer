// restartPending — tiny localStorage store tracking config sections
// saved with restart_required=true that the daemon hasn't picked up
// yet (usability arc P1.9 / A7).
//
// Lifecycle: StructuredConfigSection (and the bespoke Intelligence
// form) call markRestartPending(section) after a successful
// restart-required save. RestartPendingBanner renders the pending set
// and auto-clears it once /api/status reports a daemon started_at
// NEWER than the latest save — i.e. the operator actually restarted.
// Manual dismiss also clears (the operator saying "I know").

const KEY = "sb_restart_pending";
// Window event fired on every mutation so the banner re-renders
// without prop-drilling through the Settings tree.
export const RESTART_PENDING_EVENT = "sb-restart-pending-changed";

export type RestartPending = {
  // ISO timestamp of the most recent restart-required save.
  at: string;
  // Distinct section ids saved since the last restart/dismiss.
  sections: string[];
};

export function getRestartPending(): RestartPending | null {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as RestartPending;
    if (!parsed || typeof parsed.at !== "string" || !Array.isArray(parsed.sections)) {
      return null;
    }
    return parsed;
  } catch {
    return null;
  }
}

export function markRestartPending(section: string): void {
  const cur = getRestartPending();
  const sections = new Set(cur?.sections ?? []);
  sections.add(section);
  const next: RestartPending = {
    at: new Date().toISOString(),
    sections: [...sections].sort(),
  };
  try {
    localStorage.setItem(KEY, JSON.stringify(next));
  } catch {
    // Storage unavailable (private mode) — the banner just won't
    // persist across reloads; the save itself already showed its
    // inline restart notice.
  }
  window.dispatchEvent(new CustomEvent(RESTART_PENDING_EVENT));
}

export function clearRestartPending(): void {
  try {
    localStorage.removeItem(KEY);
  } catch {
    // ignore
  }
  window.dispatchEvent(new CustomEvent(RESTART_PENDING_EVENT));
}
