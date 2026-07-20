import { useEffect } from "react";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { num, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { Pill, StatCard, ToolBadge } from "@/components/primitives";

// relAgo renders a compact "Ns/Nm ago" from an ISO timestamp.
function relAgo(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "now";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s ago`;
  return `${Math.round(s / 60)}m ago`;
}

export function LivePage() {
  const { data, error, loading, reload } = useApi(() => api.live(), []);

  // Light auto-poll while the page is open (the window is 15 min; 10s keeps it
  // lively without hammering). Pauses when the tab is hidden.
  useEffect(() => {
    const id = window.setInterval(() => {
      if (!document.hidden) reload();
    }, 10000);
    return () => window.clearInterval(id);
  }, [reload]);

  return (
    <>
      <PageHeader
        title="Live"
        subtitle={`Sessions active in the last ${data?.window_minutes ?? 15} minutes. Audited (a view_org_sessions entry is written when this loads). Auto-refreshes every 10s.`}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : !data ? (
        <Card className="p-4">
          <Spinner label="Loading live activity…" />
        </Card>
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <StatCard label="Active sessions" value={num(data.sessions.length)} accent />
            <StatCard label="Active developers" value={num(data.active_devs)} />
            <StatCard label="Window" value={`${data.window_minutes}m`} sub={loading ? "refreshing…" : "rolling"} />
          </div>
          {data.sessions.length === 0 ? (
            <Empty message="No sessions active in the last 15 minutes." />
          ) : (
            <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
              {data.sessions.map((s) => (
                <Card key={s.session_id} className="flex items-center justify-between gap-3 p-3">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      {s.tool && <ToolBadge tool={s.tool} />}
                      <span className="truncate text-[13px] text-fg-1">{s.display_name || s.email || s.user_id}</span>
                    </div>
                    <div className="mt-1 flex items-center gap-2 text-[11px] text-fg-3">
                      <Pill variant="success">{relAgo(s.last_active)}</Pill>
                      {s.model && <span className="truncate font-mono">{s.model}</span>}
                    </div>
                  </div>
                  <div className="shrink-0 text-right">
                    <div className="font-mono text-[15px] text-fg-0">{usd(s.cost_usd)}</div>
                    <div className="text-[11px] text-fg-3">{num(s.action_count)} actions</div>
                  </div>
                </Card>
              ))}
            </div>
          )}
        </div>
      )}
    </>
  );
}
