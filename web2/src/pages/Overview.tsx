import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { num, usd } from "@/lib/format";
import { Card, ErrorState, PageHeader, Spinner, Stat } from "@/components/ui";
import { CostChart } from "@/components/CostChart";
import { WindowSelect } from "@/components/WindowSelect";

export function OverviewPage() {
  const [days, setDays] = useState(30);
  const { data, error, loading, reload } = useApi(() => api.overview(days), [days]);

  return (
    <>
      <PageHeader
        title="Overview"
        subtitle="Organisation-wide cost and activity. Aggregate only — no per-developer detail."
        right={<WindowSelect value={days} onChange={setDays} />}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <Stat label="Spend" value={usd(data.total_cost_usd)} sub={`last ${data.window_days}d`} />
            <Stat label="Active developers" value={num(data.active_developers)} />
            <Stat label="Teams" value={num(data.team_count)} />
            <Stat label="Projects" value={num(data.project_count)} />
          </div>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <Stat label="Sessions" value={num(data.total_sessions)} />
            <Stat label="Actions" value={num(data.total_actions)} />
            <Stat label="API turns" value={num(data.total_api_turns)} />
          </div>

          <Card>
            <div className="mb-3 text-sm font-medium text-fg">Spend over time</div>
            <CostChart data={data.cost_by_day} />
          </Card>

          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            <Card>
              <div className="mb-2 text-sm font-medium text-fg">Top teams</div>
              {data.top_teams.length === 0 ? (
                <div className="py-4 text-sm text-faint">No team spend.</div>
              ) : (
                <ul className="divide-y divide-line">
                  {data.top_teams.map((t) => (
                    <li key={t.team_id} className="flex items-center justify-between py-2 text-sm">
                      <Link to={`/teams/${t.team_id}`} className="text-fg hover:text-accent">
                        {t.display_name}
                      </Link>
                      <span className="font-mono text-muted">{usd(t.cost_usd)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </Card>
            <Card>
              <div className="mb-2 text-sm font-medium text-fg">Top projects</div>
              {data.top_projects.length === 0 ? (
                <div className="py-4 text-sm text-faint">No project spend.</div>
              ) : (
                <ul className="divide-y divide-line">
                  {data.top_projects.map((p) => (
                    <li key={p.project_id} className="flex items-center justify-between py-2 text-sm">
                      <Link
                        to={`/projects/${p.project_id}`}
                        className="truncate text-fg hover:text-accent"
                        title={p.project_root}
                      >
                        {p.project_root}
                      </Link>
                      <span className="ml-3 shrink-0 font-mono text-muted">{usd(p.cost_usd)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </Card>
          </div>
        </div>
      )}
    </>
  );
}
