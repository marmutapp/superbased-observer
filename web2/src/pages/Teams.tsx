import { useMemo } from "react";
import { Link } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type TeamRollup } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { num, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import {
  Sparkline,
  StatCard,
  StatStripSkeleton,
  TableSkeleton,
  ToolDot,
} from "@/components/primitives";
import { DataTable } from "@/components/DataTable";

export function TeamsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.teams(days), [days]);

  const columns = useMemo<ColumnDef<TeamRollup, any>[]>(
    () => [
      {
        accessorKey: "display_name",
        header: "Team",
        cell: (c) => (
          <Link
            to={`/teams/${c.row.original.team_id}`}
            className="font-medium text-fg-1 hover:text-accent"
          >
            {c.row.original.display_name}
          </Link>
        ),
      },
      {
        id: "tools",
        header: "Tools",
        enableSorting: false,
        cell: (c) => {
          const tools = c.row.original.top_tools ?? [];
          if (tools.length === 0) return <span className="text-fg-4">—</span>;
          return (
            <div className="flex flex-wrap items-center gap-1.5">
              {tools.map((tool) => (
                <span key={tool} className="inline-flex items-center gap-1 text-[12px] text-fg-2">
                  <ToolDot tool={tool} />
                  {tool}
                </span>
              ))}
            </div>
          );
        },
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
        accessorKey: "member_count",
        header: "Members",
        cell: (c) => num(c.row.original.member_count),
        meta: { align: "right" },
      },
      {
        accessorKey: "active_developers",
        header: "Active",
        cell: (c) => num(c.row.original.active_developers),
        meta: { align: "right" },
      },
      {
        accessorKey: "session_count",
        header: "Sessions",
        cell: (c) => num(c.row.original.session_count),
        meta: { align: "right" },
      },
      {
        accessorKey: "action_count",
        header: "Actions",
        cell: (c) => num(c.row.original.action_count),
        meta: { align: "right" },
      },
    ],
    [],
  );

  return (
    <>
      <PageHeader
        title="Teams"
        subtitle="Aggregate rollups per team. Open a team for detail; per-developer data is gated and audited."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton />
          <Card className="p-4">
            <TableSkeleton rows={5} />
          </Card>
        </div>
      ) : data.teams.length === 0 ? (
        <Empty message="No teams in scope." />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="Teams" value={num(data.teams.length)} />
            <StatCard
              label="Total spend"
              value={usd(data.teams.reduce((a, t) => a + t.cost_usd, 0))}
              accent
              helpId="tile.total_spend"
            />
            <StatCard
              label="Active devs"
              value={num(data.teams.reduce((a, t) => a + t.active_developers, 0))}
              helpId="tile.active_developers"
            />
            <StatCard label="Sessions" value={num(data.teams.reduce((a, t) => a + t.session_count, 0))} />
          </div>

          <Card className="p-3">
            <DataTable
              data={data.teams}
              columns={columns}
              rowKey={(t) => t.team_id}
              initialSort={[{ id: "cost_usd", desc: true }]}
              zebra
              minWidth={820}
            />
          </Card>
        </div>
      )}
    </>
  );
}
