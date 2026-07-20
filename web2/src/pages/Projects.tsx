import { useMemo } from "react";
import { Link } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ProjectRollup, type TokenBuckets } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, num, projectName, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import {
  Pill,
  Sparkline,
  StatCard,
  StatStripSkeleton,
  TableSkeleton,
  Tooltip,
  ToolDot,
} from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

export function ProjectsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.projects(days), [days]);

  const columns = useMemo<ColumnDef<ProjectRollup, any>[]>(
    () => [
      {
        id: "project",
        accessorFn: (p) => projectName(p.project_root),
        header: "Project",
        cell: (c) => (
          <Link
            to={`/projects/${c.row.original.project_id}`}
            className="truncate font-medium text-fg-1 hover:text-accent"
            title={c.row.original.project_root}
          >
            {projectName(c.row.original.project_root)}
          </Link>
        ),
      },
      {
        id: "teams",
        header: "Teams",
        enableSorting: false,
        cell: (c) => {
          const teams = c.row.original.teams ?? [];
          if (teams.length === 0) return <span className="text-fg-4">—</span>;
          return (
            <div className="flex flex-wrap items-center gap-1">
              {teams.map((t) => (
                <Pill key={t.team_id} variant={teams.length > 1 ? "warn" : "neutral"}>
                  {t.display_name}
                </Pill>
              ))}
            </div>
          );
        },
      },
      {
        id: "tools",
        header: "Tools",
        enableSorting: false,
        cell: (c) => {
          const tools = c.row.original.tools ?? [];
          if (tools.length === 0) return <span className="text-fg-4">—</span>;
          return (
            <div className="flex flex-wrap items-center gap-1.5">
              {tools.map((t) => (
                <span key={t} className="inline-flex items-center gap-1 text-[12px] text-fg-2">
                  <ToolDot tool={t} />
                  {t}
                </span>
              ))}
            </div>
          );
        },
      },
      {
        id: "tokens",
        header: "Tokens",
        enableSorting: false,
        cell: (c) => <TokenMiniBar buckets={c.row.original.buckets} />,
      },
      {
        id: "spark",
        header: "7d",
        enableSorting: false,
        cell: (c) => {
          const s = c.row.original.spark;
          return s && s.length >= 2 ? (
            <Sparkline data={s} width={64} height={20} />
          ) : (
            <span className="text-fg-4">—</span>
          );
        },
      },
      {
        accessorKey: "cost_usd",
        header: "Spend",
        cell: (c) => usd(c.row.original.cost_usd),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "session_count",
        header: "Sessions",
        cell: (c) => num(c.row.original.session_count),
        meta: { align: "right" },
      },
      {
        accessorKey: "active_developers",
        header: "Devs",
        cell: (c) => num(c.row.original.active_developers),
        meta: { align: "right" },
      },
    ],
    [],
  );

  return (
    <>
      <PageHeader
        title="Projects"
        subtitle="Spend per project (by git root). A project touched by more than one team is a cross-team overlap."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton />
          <Card className="p-4">
            <TableSkeleton rows={6} />
          </Card>
        </div>
      ) : data.projects.length === 0 ? (
        <Empty message="No projects in scope." />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="Projects" value={num(data.projects.length)} />
            <StatCard
              label="Total spend"
              value={usd(data.projects.reduce((a, p) => a + p.cost_usd, 0))}
              accent
              helpId="tile.total_spend"
            />
            <StatCard label="Cross-team" value={num(data.projects.filter((p) => (p.teams ?? []).length > 1).length)} />
            <StatCard label="Developers" value={num(maxDevs(data.projects))} sub="busiest project" />
          </div>

          <Card className="p-3">
            <DataTable
              data={data.projects}
              columns={columns}
              rowKey={(p) => p.project_id}
              initialSort={[{ id: "cost_usd", desc: true }]}
              zebra
              minWidth={860}
            />
          </Card>
        </div>
      )}
    </>
  );
}

// TokenMiniBar shows the net-input vs output token split for a project row as a
// two-segment proportional bar. Renders an em-dash when no tokens were captured.
function TokenMiniBar({ buckets }: { buckets?: TokenBuckets }) {
  const net = buckets?.net_input ?? 0;
  const out = buckets?.output ?? 0;
  const total = net + out;
  if (total === 0) return <span className="text-fg-4">—</span>;
  const netPct = (net / total) * 100;
  return (
    <Tooltip content={`${compact(net)} in · ${compact(out)} out`}>
      <div className="flex h-2 w-20 overflow-hidden rounded-pill bg-bg-3" tabIndex={0}>
        <div style={{ width: `${netPct}%`, background: "var(--tok-net)" }} />
        <div style={{ width: `${100 - netPct}%`, background: "var(--tok-out)" }} />
      </div>
    </Tooltip>
  );
}

function maxDevs(projects: { active_developers: number }[]): number {
  return projects.reduce((m, p) => Math.max(m, p.active_developers), 0);
}
