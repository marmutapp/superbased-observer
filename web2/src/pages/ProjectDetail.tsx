import { Link, useParams } from "react-router-dom";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { num, projectName, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { ChartShell, Pill, StatCard, ToolDot } from "@/components/primitives";
import { CostChart } from "@/components/CostChart";

export function ProjectDetailPage() {
  const { id = "" } = useParams();
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.projectDetail(id, days), [id, days]);

  return (
    <>
      <PageHeader
        title={data ? projectName(data.project_root) : "Project"}
        subtitle={data?.project_root}
        right={
          <Link to="/projects" className="text-xs text-fg-3 hover:text-fg-1">
            ← Projects
          </Link>
        }
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="Spend" value={usd(data.cost_usd)} sub={`last ${data.window_days}d`} accent />
            <StatCard label="Active developers" value={num(data.active_developers)} />
            <StatCard label="Sessions" value={num(data.session_count)} />
            <StatCard
              label="Teams"
              value={num(data.teams.length)}
              sub={data.teams.length > 1 ? "cross-team" : undefined}
            />
          </div>

          <Card>
            <div className="mb-2 text-sm font-medium text-fg-1">Teams &amp; tools</div>
            <div className="flex flex-wrap items-center gap-1.5">
              {data.teams.map((t) => (
                <Link key={t.team_id} to={`/teams/${t.team_id}`}>
                  <Pill variant={data.teams.length > 1 ? "warn" : "accent"}>{t.display_name}</Pill>
                </Link>
              ))}
              {data.tools.map((tool) => (
                <span key={tool} className="inline-flex items-center gap-1 text-[12px] text-fg-2">
                  <ToolDot tool={tool} />
                  {tool}
                </span>
              ))}
              {data.teams.length === 0 && data.tools.length === 0 && <span className="text-fg-4">—</span>}
            </div>
          </Card>

          <ChartShell title="Spend over time">
            <CostChart data={data.cost_by_day} />
          </ChartShell>

          <Card>
            <div className="mb-2 text-sm font-medium text-fg-1">Top models</div>
            {data.top_models.length === 0 ? (
              <Empty message="No model spend." />
            ) : (
              <ul className="divide-y divide-line-1">
                {data.top_models.map((m) => (
                  <li key={m.model} className="flex items-center justify-between py-2 text-sm">
                    <span className="truncate text-fg-1" title={m.model}>{m.model}</span>
                    <span className="ml-3 shrink-0 font-mono text-fg-3">{usd(m.cost_usd)}</span>
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
