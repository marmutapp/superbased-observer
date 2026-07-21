import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ToolRow } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, ms, num, pct1, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import {
  ChartShell,
  ChartSkeleton,
  StatCard,
  StatStripSkeleton,
  TableSkeleton,
  ToolBadge,
} from "@/components/primitives";
import { DataTable } from "@/components/DataTable";
import { ToolDonut } from "@/components/charts/ToolDonut";

export function ToolsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.tools(days), [days]);

  const columns = useMemo<ColumnDef<ToolRow, any>[]>(
    () => [
      {
        accessorKey: "tool",
        header: "Tool",
        cell: (c) => <ToolBadge tool={c.row.original.tool} />,
      },
      {
        accessorKey: "cost_usd",
        header: "Spend",
        cell: (c) => usd(c.row.original.cost_usd),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "tokens",
        header: "Tokens",
        cell: (c) => compact(c.row.original.tokens),
        meta: { align: "right", mono: true },
      },
      {
        accessorKey: "sessions",
        header: "Sessions",
        cell: (c) => num(c.row.original.sessions),
        meta: { align: "right" },
      },
      {
        accessorKey: "active_devs",
        header: "Devs",
        cell: (c) => num(c.row.original.active_devs),
        meta: { align: "right" },
      },
      {
        accessorKey: "action_count",
        header: "Actions",
        cell: (c) => num(c.row.original.action_count),
        meta: { align: "right" },
      },
      {
        accessorKey: "success_rate",
        header: "Success",
        cell: (c) => {
          const t = c.row.original;
          return (
            <span
              className={
                t.action_count === 0
                  ? "text-fg-4"
                  : t.success_rate < 0.95
                    ? "text-warn"
                    : "text-fg-2"
              }
            >
              {t.action_count === 0 ? "—" : pct1(t.success_rate)}
            </span>
          );
        },
        meta: { align: "right" },
      },
      {
        accessorKey: "avg_ttft_ms",
        header: "Avg TTFT",
        cell: (c) =>
          c.row.original.avg_ttft_ms > 0 ? ms(c.row.original.avg_ttft_ms) : "—",
        meta: { align: "right", mono: true },
      },
    ],
    [],
  );

  return (
    <>
      <PageHeader title="Tools" subtitle="Org-wide usage by AI tool. Aggregate, content-free." />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton />
          <Card className="p-4">
            <ChartSkeleton />
          </Card>
          <Card className="p-4">
            <TableSkeleton rows={6} />
          </Card>
        </div>
      ) : data.tools.length === 0 ? (
        <Empty message="No tool activity in this window." />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="Tools active" value={num(data.tools.length)} />
            <StatCard
              label="Total spend"
              value={usd(data.tools.reduce((a, t) => a + t.cost_usd, 0))}
              accent
              helpId="tile.total_spend"
            />
            <StatCard label="Tokens" value={compact(data.tools.reduce((a, t) => a + t.tokens, 0))} />
            <StatCard label="Actions" value={num(data.tools.reduce((a, t) => a + t.action_count, 0))} />
          </div>

          <ChartShell title="Spend by tool">
            <ToolDonut
              tools={data.tools.map((t) => ({ tool: t.tool, cost_usd: t.cost_usd, tokens: t.tokens }))}
            />
          </ChartShell>

          <Card className="p-3">
            <DataTable
              data={data.tools}
              columns={columns}
              rowKey={(t) => t.tool}
              initialSort={[{ id: "cost_usd", desc: true }]}
              zebra
              minWidth={780}
            />
          </Card>
        </div>
      )}
    </>
  );
}
