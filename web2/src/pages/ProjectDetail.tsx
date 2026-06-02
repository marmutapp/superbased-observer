import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { num, usd } from "@/lib/format";
import { Badge, Card, Empty, ErrorState, PageHeader, Spinner, Stat } from "@/components/ui";
import { CostChart } from "@/components/CostChart";
import { WindowSelect } from "@/components/WindowSelect";

export function ProjectDetailPage() {
  const { id = "" } = useParams();
  const [days, setDays] = useState(30);
  const { data, error, loading, reload } = useApi(() => api.projectDetail(id, days), [id, days]);

  return (
    <>
      <PageHeader
        title="Project"
        subtitle={data?.project_root}
        right={
          <div className="flex items-center gap-2">
            <Link to="/projects" className="text-xs text-muted hover:text-fg">
              ← Projects
            </Link>
            <WindowSelect value={days} onChange={setDays} />
          </div>
        }
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <Stat label="Spend" value={usd(data.cost_usd)} sub={`last ${data.window_days}d`} />
            <Stat label="Active developers" value={num(data.active_developers)} />
            <Stat label="Sessions" value={num(data.session_count)} />
            <Stat
              label="Teams"
              value={data.teams.length}
              sub={data.teams.length > 1 ? "cross-team" : undefined}
            />
          </div>

          <Card>
            <div className="mb-2 text-sm font-medium text-fg">Teams &amp; tools</div>
            <div className="flex flex-wrap gap-1.5">
              {data.teams.map((t) => (
                <Link key={t.team_id} to={`/teams/${t.team_id}`}>
                  <Badge tone={data.teams.length > 1 ? "warn" : "accent"}>{t.display_name}</Badge>
                </Link>
              ))}
              {data.tools.map((tool) => (
                <Badge key={tool}>{tool}</Badge>
              ))}
            </div>
          </Card>

          <Card>
            <div className="mb-3 text-sm font-medium text-fg">Spend over time</div>
            <CostChart data={data.cost_by_day} />
          </Card>

          <Card>
            <div className="mb-2 text-sm font-medium text-fg">Top models</div>
            {data.top_models.length === 0 ? (
              <Empty message="No model spend." />
            ) : (
              <ul className="divide-y divide-line">
                {data.top_models.map((m) => (
                  <li key={m.model} className="flex items-center justify-between py-2 text-sm">
                    <span className="truncate text-fg" title={m.model}>{m.model}</span>
                    <span className="ml-3 shrink-0 font-mono text-muted">{usd(m.cost_usd)}</span>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      )}
    </>
  );
}
