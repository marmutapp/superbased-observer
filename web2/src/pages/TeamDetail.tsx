import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Eye, ShieldAlert } from "lucide-react";
import { api, type DevelopersResult } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { dateTime, num, usd } from "@/lib/format";
import { Badge, Button, Card, Empty, ErrorState, PageHeader, Spinner, Stat } from "@/components/ui";
import { CostChart } from "@/components/CostChart";

export function TeamDetailPage() {
  const { id = "" } = useParams();
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.teamDetail(id, days), [id, days]);

  // Developer breakdown is OFF by default. Revealing it first records a
  // drill-down in the audit log (POST before fetch), then loads the data.
  const [devs, setDevs] = useState<DevelopersResult | null>(null);
  const [devLoading, setDevLoading] = useState(false);
  const [devError, setDevError] = useState<string | null>(null);

  async function reveal() {
    setDevLoading(true);
    setDevError(null);
    try {
      await api.logDrillDown(id); // audit BEFORE disclosure
      setDevs(await api.teamDevelopers(id, days));
    } catch (e) {
      setDevError(e instanceof Error ? e.message : String(e));
    } finally {
      setDevLoading(false);
    }
  }

  return (
    <>
      <PageHeader
        title={data?.display_name ?? "Team"}
        subtitle="Aggregate team metrics. Per-developer breakdown is gated and audited."
        right={
          <Link to="/teams" className="text-xs text-muted hover:text-fg">
            ← Teams
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
            <Stat label="Spend" value={usd(data.cost_usd)} sub={`last ${data.window_days}d`} />
            <Stat label="Active developers" value={num(data.active_developers)} sub={`${num(data.member_count)} members`} />
            <Stat label="Sessions" value={num(data.session_count)} />
            <Stat label="Actions" value={num(data.action_count)} />
          </div>

          <Card>
            <div className="mb-3 text-sm font-medium text-fg">Spend over time</div>
            <CostChart data={data.cost_by_day} />
          </Card>

          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            <Card>
              <div className="mb-2 text-sm font-medium text-fg">Top projects</div>
              {data.top_projects.length === 0 ? (
                <Empty message="No project spend." />
              ) : (
                <ul className="divide-y divide-line">
                  {data.top_projects.map((p) => (
                    <li key={p.project_id} className="flex items-center justify-between py-2 text-sm">
                      <Link to={`/projects/${p.project_id}`} className="truncate text-fg hover:text-accent" title={p.project_root}>
                        {p.project_root}
                      </Link>
                      <span className="ml-3 shrink-0 font-mono text-muted">{usd(p.cost_usd)}</span>
                    </li>
                  ))}
                </ul>
              )}
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

          {/* Privacy-gated per-developer breakdown. */}
          <Card>
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2 text-sm font-medium text-fg">
                <ShieldAlert className="h-4 w-4 text-warn" />
                Developer breakdown
              </div>
              {!devs && (
                <Button
                  variant="primary"
                  onClick={reveal}
                  disabled={devLoading}
                  title="This action will be recorded in the audit log"
                >
                  <span className="inline-flex items-center gap-1.5">
                    <Eye className="h-3.5 w-3.5" />
                    {devLoading ? "Recording…" : "Show developer breakdown"}
                  </span>
                </Button>
              )}
            </div>
            {!devs && !devError && (
              <p className="mt-2 text-xs text-faint">
                Per-developer data is hidden by default. Revealing it records a drill-down in the audit log.
              </p>
            )}
            {devError && <p className="mt-3 text-sm text-bad">{devError}</p>}
            {devs && <DeveloperTable devs={devs} />}
          </Card>
        </div>
      )}
    </>
  );
}

function DeveloperTable({ devs }: { devs: DevelopersResult }) {
  if (devs.developers.length === 0) return <Empty message="No developers on this team." />;
  return (
    <table className="mt-3 w-full text-sm">
      <thead>
        <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
          <th className="py-2 pr-3 font-medium">Developer</th>
          <th className="px-3 py-2 font-medium">Role</th>
          <th className="px-3 py-2 text-right font-medium">Spend</th>
          <th className="px-3 py-2 text-right font-medium">Sessions</th>
          <th className="px-3 py-2 text-right font-medium">Actions</th>
          <th className="px-3 py-2 text-right font-medium">Last active</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-line">
        {devs.developers.map((d) => (
          <tr key={d.user_id}>
            <td className="py-2 pr-3 text-fg">{d.display_name || d.email}</td>
            <td className="px-3 py-2">
              {d.role === "lead" ? <Badge tone="accent">lead</Badge> : <Badge>member</Badge>}
            </td>
            <td className="px-3 py-2 text-right font-mono text-fg">{usd(d.cost_usd)}</td>
            <td className="px-3 py-2 text-right text-muted">{num(d.session_count)}</td>
            <td className="px-3 py-2 text-right text-muted">{num(d.action_count)}</td>
            <td className="px-3 py-2 text-right text-muted">{d.last_active ? dateTime(d.last_active) : "—"}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
