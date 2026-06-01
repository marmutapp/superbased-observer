import { useState } from "react";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { dateTime } from "@/lib/format";
import { Badge, Button, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";

const PAGE = 50;

const ACTION_LABEL: Record<string, string> = {
  view_team_developers: "Viewed developers",
  drill_down_developers: "Drill-down",
  revoke_bearer: "Revoked bearer",
  set_team_role: "Changed role",
};

export function AuditPage() {
  const [offset, setOffset] = useState(0);
  const { data, error, loading, reload } = useApi(() => api.audit(PAGE, offset), [offset]);

  return (
    <>
      <PageHeader
        title="Audit log"
        subtitle="Every developer drill-down and admin action. Append-only, newest first."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : data.entries.length === 0 ? (
        <Empty message="No audit entries yet." />
      ) : (
        <>
          <Card className="p-0">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                  <th className="px-4 py-2.5 font-medium">When</th>
                  <th className="px-4 py-2.5 font-medium">Actor</th>
                  <th className="px-4 py-2.5 font-medium">Action</th>
                  <th className="px-4 py-2.5 font-medium">Target</th>
                  <th className="px-4 py-2.5 font-medium">Source IP</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {data.entries.map((e) => (
                  <tr key={e.id}>
                    <td className="whitespace-nowrap px-4 py-2.5 text-muted">{dateTime(e.timestamp)}</td>
                    <td className="px-4 py-2.5 text-fg">{e.actor_email || e.actor_user_id}</td>
                    <td className="px-4 py-2.5">
                      <Badge tone="accent">{ACTION_LABEL[e.action] ?? e.action}</Badge>
                    </td>
                    <td className="px-4 py-2.5 text-muted">
                      {e.target_team_id || "—"}
                      {e.target_detail ? ` · ${e.target_detail}` : ""}
                    </td>
                    <td className="px-4 py-2.5 font-mono text-faint">{e.source_ip || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
          <div className="mt-3 flex items-center justify-between text-xs text-muted">
            <Button onClick={() => setOffset(Math.max(0, offset - PAGE))} disabled={offset === 0}>
              ← Newer
            </Button>
            <span>Showing {offset + 1}–{offset + data.entries.length}</span>
            <Button onClick={() => setOffset(data.next_offset)} disabled={!data.has_more}>
              Older →
            </Button>
          </div>
        </>
      )}
    </>
  );
}
