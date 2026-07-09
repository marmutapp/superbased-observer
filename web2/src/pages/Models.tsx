import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { api, type ModelRow } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, ms, num, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import {
  ChartShell,
  ChartSkeleton,
  StatCard,
  StatStripSkeleton,
  TableSkeleton,
} from "@/components/primitives";
import { DataTable } from "@/components/DataTable";
import { ModelBar } from "@/components/charts/ModelBar";

export function ModelsPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.models(days), [days]);

  const columns = useMemo<ColumnDef<ModelRow, any>[]>(
    () => [
      {
        accessorKey: "model",
        header: "Model",
        cell: (c) => (
          <span className="truncate font-mono text-fg-1" title={c.row.original.model}>
            {c.row.original.model}
          </span>
        ),
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
        id: "cache_read",
        accessorFn: (m) => m.buckets.cache_read,
        header: "Cache read",
        cell: (c) => compact(c.row.original.buckets.cache_read),
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
      <PageHeader title="Models" subtitle="Org-wide usage by model. Aggregate, content-free." />
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
      ) : data.models.length === 0 ? (
        <Empty message="No model activity in this window." />
      ) : (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label="Models active" value={num(data.models.length)} />
            <StatCard
              label="Total spend"
              value={usd(data.models.reduce((a, m) => a + m.cost_usd, 0))}
              accent
              helpId="tile.total_spend"
            />
            <StatCard label="Tokens" value={compact(data.models.reduce((a, m) => a + m.tokens, 0))} helpId="metric.token_buckets" />
            <StatCard
              label="Cache reads"
              value={compact(data.models.reduce((a, m) => a + m.buckets.cache_read, 0))}
              helpId="metric.cache_ratio"
            />
          </div>

          <ChartShell title="Spend by model">
            <ModelBar
              models={data.models.map((m) => ({ model: m.model, cost_usd: m.cost_usd, tokens: m.tokens }))}
            />
          </ChartShell>

          <Card className="p-3">
            <DataTable
              data={data.models}
              columns={columns}
              rowKey={(m) => m.model}
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
