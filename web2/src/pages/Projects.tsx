import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { num, usd } from "@/lib/format";
import { Badge, Card, Empty, ErrorState, PageHeader, Spinner } from "@/components/ui";
import { WindowSelect } from "@/components/WindowSelect";

export function ProjectsPage() {
  const [days, setDays] = useState(30);
  const { data, error, loading, reload } = useApi(() => api.projects(days), [days]);

  return (
    <>
      <PageHeader
        title="Projects"
        subtitle="Spend per project (by git root). A project touched by more than one team is a cross-team overlap."
        right={<WindowSelect value={days} onChange={setDays} />}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : data.projects.length === 0 ? (
        <Empty message="No projects in scope." />
      ) : (
        <Card className="p-0">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                <th className="px-4 py-2.5 font-medium">Project</th>
                <th className="px-4 py-2.5 font-medium">Teams</th>
                <th className="px-4 py-2.5 font-medium">Tools</th>
                <th className="px-4 py-2.5 text-right font-medium">Spend</th>
                <th className="px-4 py-2.5 text-right font-medium">Devs</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {data.projects.map((p) => (
                <tr key={p.project_id} className="hover:bg-surface2">
                  <td className="px-4 py-2.5">
                    <Link to={`/projects/${p.project_id}`} className="truncate text-fg hover:text-accent" title={p.project_root}>
                      {p.project_root}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5">
                    <div className="flex flex-wrap items-center gap-1">
                      {p.teams.map((t) => (
                        <Badge key={t.team_id} tone={p.teams.length > 1 ? "warn" : "muted"}>
                          {t.display_name}
                        </Badge>
                      ))}
                      {p.teams.length > 1 && <span className="text-[11px] text-warn">overlap</span>}
                    </div>
                  </td>
                  <td className="px-4 py-2.5 text-muted">{p.tools.join(", ") || "—"}</td>
                  <td className="px-4 py-2.5 text-right font-mono text-fg">{usd(p.cost_usd)}</td>
                  <td className="px-4 py-2.5 text-right text-muted">{num(p.active_developers)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </>
  );
}
