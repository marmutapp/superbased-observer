import { useEffect, useRef, useState } from "react";
import { useApi } from "@/lib/useApi";
import { fetchJSON } from "@/lib/api";
import type { StatusSnapshot } from "@/lib/types";

// useDaemonRestart owns the on-demand daemon-restart flow shared by the
// RestartPendingBanner and the Settings → Health control: POST
// /api/admin/restart (the daemon runs its graceful shutdown + self re-exec),
// then poll /api/status until the NEW process answers (started_at advances past
// the click) and reload into it. The backend returns 501 when RestartFunc is
// nil (standalone `observer dashboard`, not `observer start`) — surfaced as
// `error` so callers can render honest copy.
export function useDaemonRestart() {
  const [restarting, setRestarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const restartAtRef = useRef<number>(0);

  // Poll status only while a restart is in flight; fast so the overlay clears
  // promptly once the new process is up.
  const status = useApi<StatusSnapshot>(
    restarting ? "/api/status" : null,
    undefined,
    [restarting],
    { refreshMs: 1500 },
  );

  useEffect(() => {
    if (!restarting || !status.data?.started_at) return;
    const startedAt = new Date(status.data.started_at).getTime();
    if (Number.isFinite(startedAt) && startedAt > restartAtRef.current) {
      window.location.reload();
    }
  }, [restarting, status.data?.started_at]);

  // restart optionally gates on a window.confirm message; returns nothing —
  // observe `restarting`/`error`.
  async function restart(confirmMessage?: string) {
    if (confirmMessage && !window.confirm(confirmMessage)) return;
    setError(null);
    restartAtRef.current = Date.now();
    try {
      await fetchJSON("/api/admin/restart", undefined, { method: "POST" });
      setRestarting(true);
    } catch (e) {
      setError(
        e instanceof Error && e.message
          ? e.message.replace(/^\d+\s*/, "")
          : "restart failed",
      );
    }
  }

  return { restarting, error, restart };
}
