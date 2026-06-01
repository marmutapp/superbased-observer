import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { num, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { WindowSelect } from "@/components/WindowSelect";

export function TeamsPage() {
  const [days, setDays] = useState(30);
  const { data, error, loading, reload } = useApi(() => api.teams(days), [days]);

  return (
    <>
      <PageHeader
        title="Teams"
        subtitle="Aggregate rollups per team. Open a team for detail; per-developer data is gated and audited."
        right={<WindowSelect value={days} onChange={setDays} />}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : data.teams.length === 0 ? (
        <Empty message="No teams in scope." />
      ) : (
        <Card className="p-0">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                <th className="px-4 py-2.5 font-medium">Team</th>
                <th className="px-4 py-2.5 text-right font-medium">Spend</th>
                <th className="px-4 py-2.5 text-right font-medium">Members</th>
                <th className="px-4 py-2.5 text-right font-medium">Active</th>
                <th className="px-4 py-2.5 text-right font-medium">Sessions</th>
                <th className="px-4 py-2.5 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {data.teams.map((t) => (
                <tr key={t.team_id} className="hover:bg-surface2">
                  <td className="px-4 py-2.5">
                    <Link to={`/teams/${t.team_id}`} className="text-fg hover:text-accent">
                      {t.display_name}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5 text-right font-mono text-fg">{usd(t.cost_usd)}</td>
                  <td className="px-4 py-2.5 text-right text-muted">{num(t.member_count)}</td>
                  <td className="px-4 py-2.5 text-right text-muted">{num(t.active_developers)}</td>
                  <td className="px-4 py-2.5 text-right text-muted">{num(t.session_count)}</td>
                  <td className="px-4 py-2.5 text-right text-muted">{num(t.action_count)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </>
  );
}
