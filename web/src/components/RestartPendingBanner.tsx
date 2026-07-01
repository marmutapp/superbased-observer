import { useEffect, useState } from "react";
import { useApi } from "@/lib/useApi";
import type { StatusSnapshot } from "@/lib/types";
import {
  RESTART_PENDING_EVENT,
  clearRestartPending,
  getRestartPending,
  type RestartPending,
} from "@/lib/restartPending";

// RestartPendingBanner — slim, persistent strip under the TopBar that
// appears after any restart-required config save and survives
// navigation + page reloads (usability arc P1.9). Honesty contract:
// it names the saved sections, says exactly what to do, and clears
// ITSELF when /api/status shows the daemon actually restarted (its
// started_at is newer than the last save) — no false "all applied"
// while the old process is still serving.
export function RestartPendingBanner() {
  const [pending, setPending] = useState<RestartPending | null>(
    getRestartPending,
  );
  useEffect(() => {
    const onChange = () => setPending(getRestartPending());
    window.addEventListener(RESTART_PENDING_EVENT, onChange);
    return () => window.removeEventListener(RESTART_PENDING_EVENT, onChange);
  }, []);

  // Poll status only while something is pending — zero traffic in the
  // steady state.
  const status = useApi<StatusSnapshot>(
    pending ? "/api/status" : null,
    undefined,
    [pending != null],
    { refreshMs: 15000 },
  );

  useEffect(() => {
    if (!pending || !status.data?.started_at) return;
    const startedAt = new Date(status.data.started_at).getTime();
    const savedAt = new Date(pending.at).getTime();
    if (Number.isFinite(startedAt) && Number.isFinite(savedAt) && startedAt > savedAt) {
      clearRestartPending();
    }
  }, [pending, status.data?.started_at]);

  if (!pending) return null;
  return (
    <div className="flex items-center gap-2 border-b border-warn/30 bg-warn-soft px-4 py-1.5 text-[11.5px] text-fg-2">
      <span className="font-semibold text-warn">Restart pending</span>
      <span className="min-w-0 truncate">
        saved changes to{" "}
        <span className="font-mono">{pending.sections.join(", ")}</span> apply
        on the next daemon start — restart with{" "}
        <code className="rounded-1 bg-bg-2 px-1 font-mono">observer start</code>
        {" "}(this banner clears itself once the new process is up)
      </span>
      <div className="flex-1" />
      <button
        type="button"
        onClick={clearRestartPending}
        className="shrink-0 rounded-2 border border-line-2 bg-bg-2 px-2 py-0.5 text-fg-2 hover:bg-bg-3"
      >
        dismiss
      </button>
    </div>
  );
}
