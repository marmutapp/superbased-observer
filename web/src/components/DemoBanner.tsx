import { useState } from "react";
import { useApi } from "@/lib/useApi";
import { fetchJSON } from "@/lib/api";

// DemoBanner — persistent strip while demo mode is active (usability
// arc P6.7). Deliberately NOT dismissable: it is state disclosure,
// not a notification — as long as the data surfaces serve the seeded
// sample database, the banner says so. The clear button is the
// one-click teardown (the temp database is deleted; the real one was
// never touched).
export function DemoBanner() {
  // 30s refresh keeps a second tab honest if demo was toggled
  // elsewhere; the endpoint is an in-memory bool read.
  const demo = useApi<{ available: boolean; active: boolean }>(
    "/api/demo",
    undefined,
    [],
    { refreshMs: 30000 },
  );
  const [busy, setBusy] = useState(false);
  if (!demo.data?.active) return null;

  const clear = async () => {
    setBusy(true);
    try {
      await fetchJSON("/api/demo/stop", undefined, { method: "POST" });
      window.location.reload();
    } catch {
      setBusy(false);
    }
  };

  return (
    <div className="flex items-center gap-2 border-b border-accent/30 bg-accent-soft px-4 py-1.5 text-[11.5px] text-fg-2">
      <span className="shrink-0 font-semibold text-accent">Demo data</span>
      <span className="min-w-0 truncate">
        Every data surface is showing a seeded sample. Your real database is
        untouched.
      </span>
      <div className="flex-1" />
      <button
        type="button"
        onClick={clear}
        disabled={busy}
        className="shrink-0 rounded-2 border border-line-2 bg-bg-2 px-2 py-0.5 text-fg-2 hover:bg-bg-3 disabled:opacity-60"
      >
        {busy ? "clearing…" : "Clear demo data"}
      </button>
    </div>
  );
}
